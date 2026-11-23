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
	"encoding/json"
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
	// Kept so a handler that ignores the error still can't send B or a
	// successful S.
	headerKeyErr error
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
	if err := s.ctx.Err(); err != nil {
		return wrapIfContextError(err)
	}
	return s.writeHeader(s.writeCtx)
}

func (s *websocketServerStream) Send(msg any) error {
	if err := s.ctx.Err(); err != nil {
		return wrapIfContextError(err)
	}
	body, err := marshalWebSocketBody(s.ctx, s.codec, msg, s.sendMaxBytes)
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
	var message websocketMessage
	select {
	case received, open := <-s.reader.messages:
		if !open {
			return s.reader.failure()
		}
		message = received
	case <-s.ctx.Done():
		return wrapIfContextError(context.Cause(s.ctx))
	}
	if message.marker == websocketMarkerClientEndStream {
		s.receivedEnd = true
		return io.EOF
	}
	// The reader passes on only B and C after M, so this guards against a
	// change there, not against a client.
	if message.marker != websocketMarkerBody {
		err := newWebSocketProtocolError("unexpected %c message from client", message.marker)
		s.reader.fail(err)
		return err
	}
	if err := s.codec.UnmarshalRead(s.ctx, bytes.NewReader(message.payload), msg); err != nil {
		protocolErr := newWebSocketDecodeError(err)
		s.reader.fail(protocolErr)
		return protocolErr
	}
	return nil
}

func (s *websocketServerStream) writeHeader(ctx context.Context) error {
	if s.sentHeader {
		return s.headerKeyErr
	}
	metadata := encodeWebSocketMetadata(dropInvalidWebSocketMetadata(websocketMetadata(s.info.ResponseHeader())))
	if err := writeWebSocketText(ctx, s.conn, websocketMarkerLeadingMetadata, metadata); err != nil {
		return s.writeError(err)
	}
	s.sentHeader = true
	s.headerKeyErr = websocketConnectKeyError(s.info.ResponseHeader(), "header")
	return s.headerKeyErr
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
	case peerGone:
		_ = s.conn.CloseNow()
		return
	case errors.Is(failure, errWebSocketInvalidUTF8):
		_ = s.conn.Close(websocket.StatusInvalidFramePayloadData, "")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), websocketCloseTimeout)
	defer cancel()
	headerErr := s.writeHeader(ctx)
	if headerErr != nil && !errors.Is(headerErr, errWebSocketConnectKey) {
		_ = s.conn.CloseNow()
		return
	}
	closeCode := websocket.StatusNormalClosure
	switch {
	case errors.Is(failure, errWebSocketProtocol):
		err, closeCode = failure, websocket.StatusProtocolError
	case headerErr != nil:
		err = headerErr
	default:
		if trailerErr := websocketConnectKeyError(s.info.ResponseTrailer(), "trailer"); trailerErr != nil {
			err = trailerErr
		}
	}
	payload, marshalErr := s.endStreamPayload(err)
	if marshalErr != nil {
		_ = s.conn.Close(websocket.StatusInternalError, "")
		return
	}
	if writeErr := writeWebSocketText(ctx, s.conn, websocketMarkerServerEndStream, payload); writeErr != nil {
		_ = s.conn.CloseNow()
		return
	}
	_ = s.conn.Close(closeCode, "")
}

// endStreamPayload falls back to only the error code and message over
// sendMaxBytes, as on Connect.
func (s *websocketServerStream) endStreamPayload(err error) ([]byte, error) {
	end := &connectEndStreamMessage{Trailer: dropInvalidWebSocketMetadata(websocketMetadata(s.info.ResponseTrailer()))}
	if err != nil {
		end.Error = newConnectWireError(err)
	}
	payload, marshalErr := json.Marshal(end)
	if marshalErr != nil || s.sendMaxBytes <= 0 || len(payload) <= s.sendMaxBytes {
		return payload, marshalErr
	}
	if err == nil {
		err = connect.Errorf(connect.CodeResourceExhausted, "end of stream size %d exceeds sendMaxBytes %d", len(payload), s.sendMaxBytes)
	}
	end = &connectEndStreamMessage{Error: newConnectWireError(err)}
	end.Error.Details = nil
	return json.Marshal(end)
}

// websocketServerReader keeps reading after C to notice the client going
// away, which cancels the RPC.
type websocketServerReader struct {
	conn       *websocket.Conn
	bodyIsText bool
	cancel     context.CancelCauseFunc
	messages   chan websocketMessage
	done       chan struct{}

	mu       sync.Mutex
	err      error
	peerGone bool
}

func newWebSocketServerReader(conn *websocket.Conn, bodyIsText bool, cancel context.CancelCauseFunc) *websocketServerReader {
	return &websocketServerReader{
		conn:       conn,
		bodyIsText: bodyIsText,
		cancel:     cancel,
		messages:   make(chan websocketMessage),
		done:       make(chan struct{}),
	}
}

func (r *websocketServerReader) run(ctx context.Context) {
	defer close(r.messages)
	var sawMetadata, sawEnd bool
	for {
		// coder drops the connection without a close frame when a read
		// context ends. finish closes the connection, which ends the read.
		messageType, data, err := r.conn.Read(context.WithoutCancel(ctx))
		if err != nil {
			r.connectionEnded(err)
			return
		}
		if r.isStopped() {
			// The RFC requires discarding client messages in flight after S.
			continue
		}
		message, parseErr := parseWebSocketMessage(messageType, data, r.bodyIsText)
		if parseErr != nil {
			r.fail(parseErr)
			return
		}
		// Checked here, not in Receive, so a handler that never reads still
		// gets the protocol error.
		if err := websocketClientOrderError(message.marker, sawMetadata, sawEnd); err != nil {
			r.fail(err)
			return
		}
		sawMetadata = true
		sawEnd = message.marker == websocketMarkerClientEndStream
		select {
		case r.messages <- message:
		case <-r.done:
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
	// A cause that wraps io.EOF would read as a clean end of stream.
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
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
		case !isWebSocketUpgrade(request):
			next.ServeHTTP(responseWriter, request)
		case !opts.websocket:
			http.Error(responseWriter, "WebSocket is not enabled for this procedure", http.StatusBadRequest)
		default:
			serveWebSocket(server, spec, opts, responseWriter, request)
		}
	})
}

func serveWebSocket(server *connect.Server, spec connect.Spec, opts *options, responseWriter http.ResponseWriter, request *http.Request) {
	codec, subprotocol := negotiateWebSocketCodec(request.Header, newReadOnlyCodecs(opts.codecs))
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
	timeout, hasTimeout, timeoutErr := parseWebSocketTimeout(request.URL.RawQuery)
	if hasTimeout {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, timeout)
		defer cancelTimeout()
	}
	writeCtx, cancelWrite := websocketGraceContext(ctx)
	defer cancelWrite()
	reader := newWebSocketServerReader(conn, websocketBodyIsText(codec.Name()), cancel)
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
		singleRequest: spec.StreamType == connect.StreamTypeUnary || spec.StreamType == connect.StreamTypeServer,
	}
	if timeoutErr != nil {
		stream.finish(ctx, timeoutErr)
		return
	}
	metadata, err := receiveWebSocketMetadata(ctx, reader)
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

func receiveWebSocketMetadata(ctx context.Context, reader *websocketServerReader) (http.Header, error) {
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
	case message, ok := <-reader.messages:
		if !ok {
			return nil, reader.failure()
		}
		metadata, err := decodeWebSocketMetadata(message.payload, true)
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

func negotiateWebSocketCodec(header http.Header, codecs readOnlyCodecs) (connect.Codec, string) {
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

func parseWebSocketTimeout(rawQuery string) (time.Duration, bool, error) {
	// ParseQuery skips pairs it can't parse, possibly the timeout.
	query, err := url.ParseQuery(rawQuery)
	values, ok := query[websocketTimeoutQueryParameter]
	switch {
	case !ok && err != nil && strings.Contains(rawQuery, websocketTimeoutQueryParameter):
		return 0, false, connect.Errorf(connect.CodeInvalidArgument, "parse timeout: %s", err)
	case !ok:
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

func isWebSocketUpgrade(request *http.Request) bool {
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
