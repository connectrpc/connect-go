// Copyright 2021-2026 The Connect Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connecthttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/bufferpool"
	"github.com/coder/websocket"
)

const (
	// A client that upgrades and goes quiet must not hold the connection.
	websocketFirstMessageTimeout = 10 * time.Second
	// The final M and S writes can't use the RPC context once it expires.
	websocketCloseTimeout = 5 * time.Second
)

// websocketServerStream's Receive and Send may run concurrently. finish runs
// after the handler returns.
type websocketServerStream struct {
	ctx           context.Context //nolint:containedctx // the stream is the RPC's lifetime
	writeCtx      context.Context //nolint:containedctx // ends shortly after ctx, see websocketGraceContext
	sendMaxBytes  int
	conn          *websocket.Conn
	codec         connect.Codec
	info          *connect.CallInfo
	reader        *websocketServerReader
	singleRequest bool

	receivedEnd    bool
	receivedSingle bool
	sentHeader     bool
}

func (s *websocketServerStream) Receive(msg any) error {
	if !s.singleRequest {
		return s.receive(msg)
	}
	// Clients always send C after a unary request, so read up to it.
	if s.receivedSingle {
		return io.EOF
	}
	s.receivedSingle = true
	return receiveExactlyOne(s.receive, msg, "request")
}

// SendHeaders and Send write with writeCtx so S can still follow a write in
// progress when the RPC ends, without a stalled client holding the handler.
func (s *websocketServerStream) SendHeaders() error {
	if err := context.Cause(s.ctx); err != nil {
		return wrapIfContextError(err)
	}
	return s.writeHeader(s.writeCtx)
}

func (s *websocketServerStream) Send(msg any) error {
	if err := context.Cause(s.ctx); err != nil {
		return wrapIfContextError(err)
	}
	body, err := websocketMarshalBody(s.ctx, s.codec, msg, s.sendMaxBytes)
	if err != nil {
		return err
	}
	defer bufferpool.Put(body)
	if err := s.writeHeader(s.writeCtx); err != nil {
		return err
	}
	if err := s.conn.Write(s.writeCtx, websocketBodyType(s.codec.Name()), body.Bytes()); err != nil {
		return s.writeError(err)
	}
	return nil
}

func (s *websocketServerStream) receive(msg any) error {
	if s.receivedEnd {
		return io.EOF
	}
	// Checked first, since select picks at random and a message may be queued.
	if err := context.Cause(s.ctx); err != nil {
		return wrapIfContextError(err)
	}
	var buffer *bytes.Buffer
	select {
	case received, open := <-s.reader.messages:
		if !open {
			return s.reader.failure()
		}
		buffer = received
		defer bufferpool.Put(buffer)
	case <-s.reader.endStream:
		s.receivedEnd = true
		return io.EOF
	case <-s.ctx.Done():
		return wrapIfContextError(context.Cause(s.ctx))
	}
	// The reader passes on only B after M. Skip the marker, leaving the payload.
	buffer.Next(1)
	if err := s.codec.UnmarshalRead(s.ctx, buffer, msg); err != nil {
		protocolErr := newWebSocketDecodeError(err)
		s.reader.fail(protocolErr)
		return protocolErr
	}
	return nil
}

// writeHeader fails the RPC for a connect- header, after sending M without it.
func (s *websocketServerStream) writeHeader(ctx context.Context) error {
	if s.sentHeader {
		return nil
	}
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	buffer.WriteByte(websocketMarkerLeadingMetadata)
	websocketEncodeMetadata(buffer, websocketDropInvalidMetadata(websocketMetadata(s.info.ResponseHeader())))
	if err := s.conn.Write(ctx, websocket.MessageText, buffer.Bytes()); err != nil {
		return s.writeError(err)
	}
	s.sentHeader = true
	if err := websocketValidateResponseMetadata(s.info.ResponseHeader(), "header"); err != nil {
		s.reader.fail(err)
		return err
	}
	return nil
}

func (s *websocketServerStream) writeError(err error) error {
	err = wrapIfContextDone(s.ctx, err)
	if _, ok := asError(err); ok {
		return err
	}
	return connect.Errorf(connect.CodeUnavailable, "write message: %s", err).WithCause(err)
}

func (s *websocketServerStream) finish(ctx context.Context, err error) {
	peerGone, failure := s.reader.beginEndStream()
	switch {
	case errors.Is(failure, websocket.ErrMessageTooBig):
		// coder already sent 1009. Close drains the rest of the message, so
		// the socket isn't reset with the Close frame unread.
		_ = s.conn.Close(websocket.StatusMessageTooBig, "")
		return
	case peerGone:
		_ = s.conn.CloseNow()
		return
	case errors.Is(failure, errWebSocketInvalidUTF8):
		_ = s.conn.Close(websocket.StatusInvalidFramePayloadData, "")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), websocketCloseTimeout)
	defer cancel()
	if s.writeHeader(ctx) != nil && !s.sentHeader {
		_ = s.conn.CloseNow()
		return
	}
	closeCode := websocket.StatusNormalClosure
	// Read again, since writeHeader can fail the RPC.
	if failure := s.reader.failure(); failure != nil {
		err = failure
		if errors.Is(failure, errWebSocketProtocol) {
			closeCode = websocket.StatusProtocolError
		}
	} else if trailerErr := websocketValidateResponseMetadata(s.info.ResponseTrailer(), "trailer"); trailerErr != nil {
		err = trailerErr
	}
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	if s.encodeEndStream(buffer, err) != nil {
		_ = s.conn.Close(websocket.StatusInternalError, "")
		return
	}
	if writeErr := s.conn.Write(ctx, websocket.MessageText, buffer.Bytes()); writeErr != nil {
		_ = s.conn.CloseNow()
		return
	}
	_ = s.conn.Close(closeCode, "")
}

// encodeEndStream writes S into buffer. Over sendMaxBytes it falls back to
// only the error code and message, as on Connect.
func (s *websocketServerStream) encodeEndStream(buffer *bytes.Buffer, err error) error {
	end := &connectEndStreamMessage{Trailer: websocketDropInvalidMetadata(websocketMetadata(s.info.ResponseTrailer()))}
	if err != nil {
		end.Error = newConnectWireError(err)
	}
	buffer.WriteByte(websocketMarkerServerEndStream)
	marshalErr := websocketAppendJSON(buffer, end)
	if size := buffer.Len() - 1; marshalErr != nil || s.sendMaxBytes <= 0 || size <= s.sendMaxBytes {
		return marshalErr
	} else if err == nil {
		err = connect.Errorf(connect.CodeResourceExhausted, "end of stream size %d exceeds sendMaxBytes %d", size, s.sendMaxBytes)
	}
	end = &connectEndStreamMessage{Error: newConnectWireError(err)}
	end.Error.Details = nil
	buffer.Truncate(1)
	return websocketAppendJSON(buffer, end)
}

// websocketServerReader keeps reading after C to notice the client going
// away, which cancels the RPC.
type websocketServerReader struct {
	conn     *websocket.Conn
	bodyType websocket.MessageType
	cancel   context.CancelCauseFunc
	// messages carries whole M and B messages, marker included.
	messages chan *bytes.Buffer
	// endStream closes on C, so the reader doesn't wait for the handler.
	endStream chan struct{}
	done      chan struct{}

	mu       sync.Mutex
	err      error
	peerGone bool
}

func (r *websocketServerReader) run(ctx context.Context) {
	defer close(r.messages)
	var sawMetadata, sawEnd bool
	for {
		// coder drops the connection without a close frame when a read
		// context ends. finish closes the connection, which ends the read.
		buffer := bufferpool.Get()
		messageType, err := websocketReadMessage(context.WithoutCancel(ctx), r.conn, buffer)
		if err != nil {
			bufferpool.Put(buffer)
			r.connectionEnded(err)
			return
		}
		if r.isStopped() {
			// The RFC requires discarding client messages in flight after S.
			bufferpool.Put(buffer)
			continue
		}
		marker, parseErr := websocketParseMarker(messageType, buffer.Bytes(), r.bodyType)
		// Checked here, so a message after C fails the RPC even if the
		// handler stopped reading.
		if parseErr == nil {
			parseErr = websocketClientOrderError(marker, sawMetadata, sawEnd)
		}
		if parseErr != nil {
			bufferpool.Put(buffer)
			r.fail(parseErr)
			return
		}
		sawMetadata = true
		if marker == websocketMarkerClientEndStream {
			bufferpool.Put(buffer)
			sawEnd = true
			close(r.endStream)
			continue
		}
		select {
		case r.messages <- buffer:
		case <-r.done:
			bufferpool.Put(buffer)
		}
	}
}

// websocketClientOrderError follows the server's state table in RFC 008
// Section 6.2.
func websocketClientOrderError(marker byte, sawMetadata, sawEnd bool) *connect.Error {
	switch {
	case sawEnd:
		return newWebSocketProtocolError("%c message after C", marker)
	case !sawMetadata && marker != websocketMarkerLeadingMetadata:
		return newWebSocketProtocolError("first message is %c, expected M", marker)
	case sawMetadata && marker != websocketMarkerBody && marker != websocketMarkerClientEndStream:
		return newWebSocketProtocolError("unexpected %c message from client", marker)
	default:
		return nil
	}
}

func (r *websocketServerReader) fail(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
	r.cancel(err)
}

// connectionEnded before S means the client canceled, or coder closed with
// 1009 for a message over the read limit.
func (r *websocketServerReader) connectionEnded(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isStopped() {
		return
	}
	r.peerGone = true
	err = websocketCause(err)
	if r.err == nil {
		r.err = connect.Errorf(connect.CodeCanceled, "client closed the connection: %s", err).WithCause(err)
		if errors.Is(err, websocket.ErrMessageTooBig) {
			r.err = connect.Errorf(connect.CodeResourceExhausted, "read message: %s", err).WithCause(err)
		}
	}
	r.cancel(r.err)
}

// beginEndStream must be called once.
func (r *websocketServerReader) beginEndStream() (peerGone bool, failure error) {
	close(r.done)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peerGone, r.err
}

func (r *websocketServerReader) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *websocketServerReader) isStopped() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func newWebSocketHandler(server *connect.Server, spec connect.Spec, opts *options, next http.Handler) http.Handler {
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		switch {
		case !websocketIsUpgrade(request):
			next.ServeHTTP(responseWriter, request)
		case !opts.websocket:
			http.Error(responseWriter, "WebSocket is not enabled for this procedure", http.StatusBadRequest)
		default:
			websocketServe(server, spec, opts, responseWriter, request)
		}
	})
}

func websocketServe(server *connect.Server, spec connect.Spec, opts *options, responseWriter http.ResponseWriter, request *http.Request) {
	codec, subprotocol := websocketNegotiateCodec(request.Header, newReadOnlyCodecs(opts.codecs))
	if codec == nil {
		http.Error(responseWriter, "no supported Connect subprotocol offered", http.StatusBadRequest)
		return
	}
	// Accept checks the Origin and writes 403 for a cross-origin handshake.
	conn, err := websocket.Accept(responseWriter, request, &websocket.AcceptOptions{
		Subprotocols: []string{subprotocol},
	})
	if err != nil {
		return
	}
	// net/http doesn't close hijacked connections, even after a panic.
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(websocketReadLimit(opts.readMaxBytes))

	ctx, cancel := context.WithCancelCause(request.Context())
	defer cancel(nil)
	timeout, hasTimeout, timeoutErr := websocketParseTimeout(request.URL.RawQuery)
	if hasTimeout {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, timeout)
		defer cancelTimeout()
	}
	writeCtx, cancelWrite := websocketGraceContext(ctx)
	defer cancelWrite()
	reader := &websocketServerReader{
		conn:      conn,
		bodyType:  websocketBodyType(codec.Name()),
		cancel:    cancel,
		messages:  make(chan *bytes.Buffer),
		endStream: make(chan struct{}),
		done:      make(chan struct{}),
	}
	go reader.run(ctx)

	info := &connect.CallInfo{TransportInfo: &ServerInfo{request: request}}
	info.Spec = spec
	info.PeerAddr = request.RemoteAddr
	info.Protocol = protocolNameWebSocket
	info.Codec = codec.Name()
	info.RequestEncoding = connect.CompressionNameIdentity
	info.ResponseEncoding = connect.CompressionNameIdentity
	stream := &websocketServerStream{
		ctx:           ctx,
		writeCtx:      writeCtx,
		sendMaxBytes:  opts.sendMaxBytes,
		conn:          conn,
		codec:         codec,
		info:          info,
		reader:        reader,
		singleRequest: spec.StreamType&connect.StreamTypeClient == 0,
	}
	if timeoutErr != nil {
		stream.finish(ctx, timeoutErr)
		return
	}
	metadata, err := websocketReceiveMetadata(ctx, reader)
	if err != nil {
		stream.finish(ctx, err)
		return
	}
	header, headerErr := websocketRequestHeader(request, metadata)
	if headerErr != nil {
		reader.fail(headerErr)
		stream.finish(ctx, headerErr)
		return
	}
	fromHTTPHeader(info.RequestHeader(), header)
	stream.finish(ctx, scrubHandlerError(server.Call(ctx, spec.Procedure, info, stream)))
}

func websocketReceiveMetadata(ctx context.Context, reader *websocketServerReader) (http.Header, error) {
	// Don't let select race an expired zero timeout against a message.
	if err := ctx.Err(); err != nil {
		return nil, wrapIfContextError(err)
	}
	timer := time.NewTimer(websocketFirstMessageTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, wrapIfContextError(ctx.Err())
	case <-timer.C:
		return nil, connect.Errorf(connect.CodeDeadlineExceeded, "no M message within %s", websocketFirstMessageTimeout)
	case buffer, ok := <-reader.messages:
		if !ok {
			return nil, reader.failure()
		}
		defer bufferpool.Put(buffer)
		// Skip the marker, leaving the payload.
		buffer.Next(1)
		metadata, err := websocketDecodeMetadata(buffer.Bytes(), true)
		if err != nil {
			reader.fail(err)
			return nil, err
		}
		return metadata, nil
	}
}

// websocketRequestHeader builds the RPC's effective headers. Metadata can't
// replace a handshake header, which a proxy may have set.
func websocketRequestHeader(request *http.Request, metadata http.Header) (http.Header, *connect.Error) {
	header := make(http.Header, len(request.Header)+len(metadata)+1)
	for key, values := range request.Header {
		if key == "Connection" || key == "Upgrade" ||
			strings.HasPrefix(key, "Sec-Websocket-") || strings.HasPrefix(key, "Connect-") {
			continue
		}
		header[key] = values
	}
	header.Set(headerHost, request.Host)
	for key, values := range metadata {
		if _, ok := request.Header[key]; ok {
			return nil, newWebSocketProtocolError("M message key %q is also a handshake header", strings.ToLower(key))
		}
		header[key] = values
	}
	return header, nil
}

func websocketNegotiateCodec(header http.Header, codecs readOnlyCodecs) (connect.Codec, string) {
	for _, value := range header.Values("Sec-WebSocket-Protocol") {
		for token := range strings.SplitSeq(value, ",") {
			token = strings.TrimSpace(token)
			name, ok := strings.CutPrefix(token, websocketSubprotocolPrefix)
			if !ok {
				continue
			}
			if codec := codecs.Get(name); codec != nil {
				return codec, token
			}
		}
	}
	return nil, ""
}

func websocketParseTimeout(rawQuery string) (time.Duration, bool, error) {
	// Scanned by hand, since ParseQuery skips a pair it can't unescape.
	var values []string
	for pair := range strings.SplitSeq(rawQuery, "&") {
		key, value, _ := strings.Cut(pair, "=")
		if key, err := url.QueryUnescape(key); err != nil || key != websocketTimeoutQueryParameter {
			continue
		}
		value, err := url.QueryUnescape(value)
		if err != nil {
			return 0, false, connect.Errorf(connect.CodeInvalidArgument, "parse timeout: %s", err)
		}
		values = append(values, value)
	}
	switch {
	case len(values) == 0:
		return 0, false, nil
	case len(values) > 1:
		return 0, false, connect.NewError(connect.CodeInvalidArgument, "parse timeout: more than one value supplied")
	case values[0] == "":
		return 0, false, connect.NewError(connect.CodeInvalidArgument, "parse timeout: no value supplied")
	}
	timeout, err := connectParseTimeout(values[0])
	if err != nil {
		return 0, false, err
	}
	return timeout, true, nil
}

func websocketIsUpgrade(request *http.Request) bool {
	return request.Method == http.MethodGet &&
		headerHasToken(request.Header, "Connection", "upgrade") &&
		headerHasToken(request.Header, "Upgrade", "websocket")
}

func headerHasToken(header http.Header, key, token string) bool {
	for _, value := range header.Values(key) {
		for field := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(field), token) {
				return true
			}
		}
	}
	return false
}
