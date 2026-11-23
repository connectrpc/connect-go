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
// when the handler returns or the RPC's context ends, whichever is first.
// Receive reads the connection until C, then closeRead reads on until S.
type websocketServerStream struct {
	ctx           context.Context //nolint:containedctx // the stream is the RPC's lifetime
	writeCtx      context.Context //nolint:containedctx // ends shortly after ctx, see websocketGraceContext
	cancel        context.CancelCauseFunc
	sendMaxBytes  int
	conn          *websocket.Conn
	codec         connect.Codec
	info          *connect.CallInfo
	singleRequest bool

	// finish closes done, so closeRead discards what arrives after S.
	done chan struct{}

	// writeMu orders Send against finish, which can run from a context
	// callback while the handler runs.
	writeMu sync.Mutex
	ended   bool

	sawMetadata bool
	receivedEnd bool
	sentHeader  bool
}

func (s *websocketServerStream) Receive(msg any) error {
	if !s.singleRequest {
		return s.receive(msg)
	}
	// Read up to C, which starts closeRead before the handler runs.
	if s.receivedEnd {
		return io.EOF
	}
	return receiveExactlyOne(s.receive, msg, "request")
}

// SendHeaders and Send write with writeCtx so S can still follow a write in
// progress when the RPC ends, without a stalled client holding the handler.
func (s *websocketServerStream) SendHeaders() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := context.Cause(s.ctx); err != nil || s.ended {
		return wrapIfContextError(err)
	}
	return s.writeHeader(s.writeCtx)
}

func (s *websocketServerStream) Send(msg any) error {
	if err := context.Cause(s.ctx); err != nil {
		return wrapIfContextError(err)
	}
	body := bufferpool.Get()
	defer bufferpool.Put(body)
	if err := websocketMarshalBody(s.ctx, body, s.codec, msg, s.sendMaxBytes); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := context.Cause(s.ctx); err != nil || s.ended {
		return wrapIfContextError(err)
	}
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
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	marker, err := s.readMessage(s.ctx, buffer)
	if err != nil {
		return err
	}
	if marker == websocketMarkerClientEndStream {
		s.receivedEnd = true
		go s.closeRead(s.ctx)
		return io.EOF
	}
	// Skip the marker, leaving the payload.
	buffer.Next(1)
	if err := s.codec.UnmarshalRead(s.ctx, buffer, msg); err != nil {
		protocolErr := newWebSocketDecodeError(err)
		s.cancel(protocolErr)
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
		s.cancel(err)
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

// finish writes the RPC's outcome. A failure recorded as the context's cause
// replaces the handler's error. A deadline is only a cause, so the handler's
// error stands.
func (s *websocketServerStream) finish(ctx context.Context, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	close(s.done)
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, websocket.ErrMessageTooBig):
		// coder already sent 1009. Close drains the rest of the message, so
		// the socket isn't reset with the Close frame unread.
		_ = s.conn.Close(websocket.StatusMessageTooBig, "")
		return
	case errors.Is(cause, errWebSocketInvalidUTF8):
		_ = s.conn.Close(websocket.StatusInvalidFramePayloadData, "")
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), websocketCloseTimeout)
	defer cancel()
	// Fails once the client has gone.
	if s.writeHeader(writeCtx) != nil && !s.sentHeader {
		_ = s.conn.CloseNow()
		return
	}
	closeCode := websocket.StatusNormalClosure
	// Read after writeHeader, which can fail the RPC.
	if cause, ok := asError(context.Cause(ctx)); ok {
		err = cause
		if errors.Is(cause, errWebSocketProtocol) {
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
	if writeErr := s.conn.Write(writeCtx, websocket.MessageText, buffer.Bytes()); writeErr != nil {
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

// readMessage reads one message into buffer and checks it against the
// server's state table.
func (s *websocketServerStream) readMessage(ctx context.Context, buffer *bytes.Buffer) (byte, error) {
	// coder drops the connection when a read's context ends. The end of ctx
	// runs finish instead, which closes the connection and ends the read.
	messageType, err := websocketReadMessage(context.WithoutCancel(ctx), s.conn, buffer)
	if err != nil {
		if ctxErr := context.Cause(ctx); ctxErr != nil {
			return 0, wrapIfContextError(ctxErr)
		}
		return 0, s.connectionEnded(err)
	}
	marker, parseErr := websocketParseMarker(messageType, buffer.Bytes(), websocketBodyType(s.codec.Name()))
	if parseErr == nil {
		parseErr = websocketClientOrderError(marker, s.sawMetadata, s.receivedEnd)
	}
	if parseErr != nil {
		s.cancel(parseErr)
		return 0, parseErr
	}
	s.sawMetadata = true
	return marker, nil
}

// closeRead reads after C until S, as RFC 008 Section 8.1 requires, so the
// end of the connection or a message after C fails the RPC.
func (s *websocketServerStream) closeRead(ctx context.Context) {
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	for {
		buffer.Reset()
		// finish closes the connection, which ends the read.
		messageType, err := websocketReadMessage(context.WithoutCancel(ctx), s.conn, buffer)
		select {
		case <-s.done:
			// The RFC requires discarding client messages in flight after S.
			if err == nil {
				continue
			}
			return
		default:
		}
		if err != nil {
			_ = s.connectionEnded(err)
			return
		}
		marker, parseErr := websocketParseMarker(messageType, buffer.Bytes(), websocketBodyType(s.codec.Name()))
		if parseErr == nil {
			parseErr = websocketClientOrderError(marker, true, true)
		}
		s.cancel(parseErr)
		return
	}
}

// connectionEnded before S means the client canceled, or coder closed with
// 1009 for a message over the read limit.
func (s *websocketServerStream) connectionEnded(err error) error {
	err = websocketCause(err)
	if errors.Is(err, websocket.ErrMessageTooBig) {
		err = connect.Errorf(connect.CodeResourceExhausted, "read message: %s", err).WithCause(err)
	} else {
		err = connect.Errorf(connect.CodeCanceled, "client closed the connection: %s", err).WithCause(err)
	}
	s.cancel(err)
	return err
}

func (s *websocketServerStream) receiveMetadata(ctx context.Context) (http.Header, error) {
	if err := ctx.Err(); err != nil {
		return nil, wrapIfContextError(err)
	}
	readCtx, cancel := context.WithTimeoutCause(ctx, websocketFirstMessageTimeout,
		connect.Errorf(connect.CodeDeadlineExceeded, "no M message within %s", websocketFirstMessageTimeout))
	defer cancel()
	stop := context.AfterFunc(readCtx, func() {
		s.finish(ctx, wrapIfContextError(context.Cause(readCtx)))
	})
	defer stop()
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	if _, err := s.readMessage(readCtx, buffer); err != nil {
		if cause := context.Cause(readCtx); cause != nil && ctx.Err() == nil {
			return nil, cause
		}
		return nil, err
	}
	// Skip the marker, leaving the payload.
	buffer.Next(1)
	metadata, err := websocketDecodeMetadata(buffer.Bytes(), true)
	if err != nil {
		s.cancel(err)
		return nil, err
	}
	return metadata, nil
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
		cancel:        cancel,
		sendMaxBytes:  opts.sendMaxBytes,
		conn:          conn,
		codec:         codec,
		info:          info,
		singleRequest: spec.StreamType&connect.StreamTypeClient == 0,
		done:          make(chan struct{}),
	}
	// A deadline or failure writes S now, without waiting for the handler.
	stop := context.AfterFunc(ctx, func() {
		stream.finish(ctx, wrapIfContextError(context.Cause(ctx)))
	})
	defer stop()
	if timeoutErr != nil {
		stream.finish(ctx, timeoutErr)
		return
	}
	metadata, err := stream.receiveMetadata(ctx)
	if err != nil {
		stream.finish(ctx, err)
		return
	}
	header, headerErr := websocketRequestHeader(request, metadata)
	if headerErr != nil {
		stream.cancel(headerErr)
		stream.finish(ctx, headerErr)
		return
	}
	fromHTTPHeader(info.RequestHeader(), header)
	stream.finish(ctx, scrubHandlerError(server.Call(ctx, spec.Procedure, info, stream)))
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
