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
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/bufferpool"
	"github.com/coder/websocket"
)

const (
	websocketFirstMessageTimeout = 10 * time.Second
	websocketCloseTimeout        = 5 * time.Second
	// The server pings after writing nothing for this long, below the common
	// 60s idle timeout of proxies and load balancers.
	websocketPingInterval = 30 * time.Second
)

// websocketServerStream's Receive and Send may run concurrently. read owns
// the connection's reads and hands each message to Receive. finish runs once,
// after the handler returns or when the RPC fails before dispatch. Writes end
// websocketCloseGrace after ctx.
type websocketServerStream struct {
	ctx           context.Context //nolint:containedctx // the stream is the RPC's lifetime
	ioCtx         context.Context //nolint:containedctx // ends shortly after ctx, see websocketGraceContext
	cancel        context.CancelCauseFunc
	sendMaxBytes  int
	conn          *websocket.Conn
	netConn       net.Conn
	codec         connect.Codec
	info          *connect.CallInfo
	singleRequest bool
	messages      chan websocketMessage

	peerGone          atomic.Bool
	lastWriteUnixNano atomic.Int64

	receivedEnd bool
	sentHeader  bool
}

type websocketMessage struct {
	marker byte
	buffer *bytes.Buffer
}

func (s *websocketServerStream) Receive(msg any) error {
	if !s.singleRequest {
		return s.receive(msg)
	}
	if s.receivedEnd {
		return io.EOF
	}
	return receiveExactlyOne(s.receive, msg, "request")
}

func (s *websocketServerStream) SendHeaders() error {
	if err := context.Cause(s.ctx); err != nil {
		return wrapIfContextError(err)
	}
	return s.writeHeader(s.ioCtx, s.info.ResponseHeader())
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
	if err := s.writeHeader(s.ioCtx, s.info.ResponseHeader()); err != nil {
		return err
	}
	if err := s.write(s.ioCtx, websocketBodyType(s.codec.Name()), body.Bytes()); err != nil {
		return s.writeError(err)
	}
	return nil
}

func (s *websocketServerStream) receive(msg any) error {
	if s.receivedEnd {
		return io.EOF
	}
	if err := context.Cause(s.ctx); err != nil {
		return wrapIfContextError(err)
	}
	var message websocketMessage
	var more bool
	select {
	case message, more = <-s.messages:
	case <-s.ctx.Done():
		return wrapIfContextError(context.Cause(s.ctx))
	}
	if !more {
		s.receivedEnd = true
		return io.EOF
	}
	defer bufferpool.Put(message.buffer)
	if message.marker != websocketMarkerBody {
		return s.fail(newWebSocketProtocolError("unexpected %c message from client", message.marker))
	}
	if err := s.codec.UnmarshalRead(s.ctx, message.buffer, msg); err != nil {
		return s.fail(newWebSocketDecodeError(err))
	}
	return nil
}

// writeHeader sends M with the response headers if it hasn't been sent. A
// connect- header is left out of M and then fails the RPC.
func (s *websocketServerStream) writeHeader(ctx context.Context, responseHeader *connect.Header) error {
	if s.sentHeader {
		return nil
	}
	header, headerErr := websocketResponseMetadata(responseHeader, "header")
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	websocketMarshalMetadata(buffer, header)
	if err := s.write(ctx, websocket.MessageText, buffer.Bytes()); err != nil {
		return s.writeError(err)
	}
	s.sentHeader = true
	if headerErr != nil {
		return s.fail(headerErr)
	}
	return nil
}

func (s *websocketServerStream) write(ctx context.Context, messageType websocket.MessageType, data []byte) error {
	err := s.conn.Write(ctx, messageType, data)
	s.lastWriteUnixNano.Store(time.Now().UnixNano())
	return err
}

func (s *websocketServerStream) writeError(err error) error {
	if s.ctx.Err() != nil {
		return wrapIfContextDone(s.ctx, err)
	}
	return s.connectionEnded(err)
}

// finish writes the RPC's outcome. A failure recorded as the context's cause
// replaces the handler's error, and so does an expired deadline.
func (s *websocketServerStream) finish(ctx context.Context, err error) {
	s.cancel(errWebSocketFinished)
	cause := context.Cause(ctx)
	if errors.Is(cause, errWebSocketFinished) {
		cause = nil
	}
	if s.peerGone.Load() {
		// RFC 008 Section 8: no S once the connection has ended.
		_ = s.conn.CloseNow()
		return
	}
	switch {
	case errors.Is(cause, websocket.ErrMessageTooBig):
		// coder already sent 1009. Close would drain the rest of the message
		// with no deadline and hang on a client that stalls.
		_ = s.conn.CloseNow()
		return
	case errors.Is(cause, errWebSocketInvalidUTF8):
		s.close(websocket.StatusInvalidFramePayloadData)
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), websocketCloseTimeout)
	defer cancel()
	if headerErr := s.writeHeader(writeCtx, s.info.ResponseHeader()); headerErr != nil {
		if !s.sentHeader {
			_ = s.conn.CloseNow()
			return
		}
		if cause == nil {
			cause = headerErr
		}
	}
	closeCode := websocket.StatusNormalClosure
	trailer, trailerErr := websocketResponseMetadata(s.info.ResponseTrailer(), "trailer")
	if causeErr, ok := asError(cause); ok {
		err = causeErr
		if errors.Is(causeErr, errWebSocketProtocol) {
			closeCode = websocket.StatusProtocolError
		}
	} else if errors.Is(cause, context.DeadlineExceeded) {
		// RFC 008 Section 11.1, even if the handler returned nil.
		err = wrapIfContextError(cause)
	} else if trailerErr != nil {
		err = trailerErr
	}
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	if s.encodeEndStream(buffer, trailer, err) != nil {
		s.close(websocket.StatusInternalError)
		return
	}
	if writeErr := s.conn.Write(writeCtx, websocket.MessageText, buffer.Bytes()); writeErr != nil {
		_ = s.conn.CloseNow()
		return
	}
	s.close(closeCode)
}

// close bounds the close handshake with a deadline on the connection. coder
// otherwise waits with no deadline for the rest of a message the client is
// sending.
func (s *websocketServerStream) close(code websocket.StatusCode) {
	_ = s.netConn.SetDeadline(time.Now().Add(websocketCloseTimeout))
	_ = s.conn.Close(code, "")
}

// encodeEndStream writes S into buffer. Over sendMaxBytes it falls back to
// only the error code and message, as on Connect.
func (s *websocketServerStream) encodeEndStream(buffer *bytes.Buffer, trailer http.Header, rpcErr error) error {
	end := &connectEndStreamMessage{Trailer: trailer}
	if rpcErr != nil {
		end.Error = newConnectWireError(rpcErr)
	}
	buffer.WriteByte(websocketMarkerServerEndStream)
	if err := websocketAppendJSON(buffer, end); err != nil {
		return err
	}
	size := buffer.Len() - 1
	if s.sendMaxBytes <= 0 || size <= s.sendMaxBytes {
		return nil
	}
	if rpcErr == nil {
		rpcErr = connect.Errorf(connect.CodeResourceExhausted, "end of stream size %d exceeds sendMaxBytes %d", size, s.sendMaxBytes)
	}
	end = &connectEndStreamMessage{Error: newConnectWireError(rpcErr)}
	end.Error.Details = nil
	// Discard all but the marker byte.
	buffer.Truncate(1)
	return websocketAppendJSON(buffer, end)
}

// read reads the connection until it ends, which finish does on close. It
// raises the read limit after M and closes messages at C. A message after C
// fails the RPC, as RFC 008 Section 8.1 requires.
func (s *websocketServerStream) read(readMaxBytes int) {
	receivedEnd := false
	for {
		buffer := bufferpool.Get()
		marker, err := s.readMessage(buffer)
		if err == nil && receivedEnd {
			err = s.fail(newWebSocketProtocolError("%c message after C", marker))
		}
		if err != nil {
			bufferpool.Put(buffer)
			return
		}
		if marker == websocketMarkerLeadingMetadata {
			s.conn.SetReadLimit(websocketReadLimit(readMaxBytes))
		}
		if marker == websocketMarkerClientEndStream {
			bufferpool.Put(buffer)
			close(s.messages)
			receivedEnd = true
			continue
		}
		select {
		case s.messages <- websocketMessage{marker: marker, buffer: buffer}:
		case <-s.ctx.Done():
			bufferpool.Put(buffer)
			return
		}
	}
}

func (s *websocketServerStream) readMessage(buffer *bytes.Buffer) (byte, error) {
	messageType, err := websocketReadMessage(context.WithoutCancel(s.ctx), s.conn, buffer)
	if err != nil {
		return 0, s.connectionEnded(err)
	}
	marker, parseErr := websocketParseMarker(messageType, buffer, s.codec.Name())
	if parseErr != nil {
		return 0, s.fail(parseErr)
	}
	return marker, nil
}

// connectionEnded before S means the client canceled, or coder closed with
// 1009 for a message over the read limit. It returns the RPC's first cause.
func (s *websocketServerStream) connectionEnded(err error) error {
	err = websocketCause(err)
	if errors.Is(err, websocket.ErrMessageTooBig) {
		err = connect.Errorf(connect.CodeResourceExhausted, "read message: %s", err).WithCause(err)
	} else {
		s.peerGone.Store(true)
		err = connect.Errorf(connect.CodeCanceled, "client closed the connection: %s", err).WithCause(err)
	}
	s.cancel(err)
	return wrapIfContextError(context.Cause(s.ctx))
}

// receiveMetadata waits for M, failing the RPC if it doesn't arrive in time.
func (s *websocketServerStream) receiveMetadata() (http.Header, error) {
	timer := time.NewTimer(websocketFirstMessageTimeout)
	defer timer.Stop()
	var message websocketMessage
	var more bool
	select {
	case message, more = <-s.messages:
	case <-timer.C:
		return nil, s.fail(connect.Errorf(connect.CodeDeadlineExceeded, "no M message within %s", websocketFirstMessageTimeout))
	case <-s.ctx.Done():
		return nil, wrapIfContextError(context.Cause(s.ctx))
	}
	if !more {
		return nil, s.fail(newWebSocketProtocolError("first message is C, expected M"))
	}
	defer bufferpool.Put(message.buffer)
	if message.marker != websocketMarkerLeadingMetadata {
		return nil, s.fail(newWebSocketProtocolError("first message is %c, expected M", message.marker))
	}
	metadata, decodeErr := websocketDecodeMetadata(message.buffer.Bytes(), true)
	if decodeErr != nil {
		return nil, s.fail(decodeErr)
	}
	return metadata, nil
}

// fail records err as the RPC's cause, which finish writes in S.
func (s *websocketServerStream) fail(err error) error {
	s.cancel(err)
	return err
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
	hijacker := &websocketHijacker{ResponseWriter: responseWriter}
	// Accept checks the Origin and writes 403 for a cross-origin handshake.
	conn, err := websocket.Accept(hijacker, request, &websocket.AcceptOptions{
		Subprotocols: []string{subprotocol},
	})
	if err != nil {
		return
	}
	// net/http doesn't close hijacked connections, even after a panic.
	defer func() { _ = conn.CloseNow() }()
	// M is bounded like HTTP headers, whatever the message read limit.
	conn.SetReadLimit(websocketReadLimit(http.DefaultMaxHeaderBytes))

	ctx, cancel := context.WithCancelCause(request.Context())
	defer cancel(nil)
	timeout, hasTimeout, timeoutErr := websocketParseTimeout(request.URL.RawQuery)
	if hasTimeout {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, timeout)
		defer cancelTimeout()
	}
	ioCtx, cancelIO := websocketGraceContext(ctx)
	defer cancelIO()
	info := &connect.CallInfo{TransportInfo: &ServerInfo{request: request}}
	info.Spec = spec
	info.PeerAddr = request.RemoteAddr
	info.Protocol = protocolNameWebSocket
	info.Codec = codec.Name()
	info.RequestEncoding = connect.CompressionNameIdentity
	info.ResponseEncoding = connect.CompressionNameIdentity
	stream := &websocketServerStream{
		ctx:           ctx,
		ioCtx:         ioCtx,
		cancel:        cancel,
		sendMaxBytes:  opts.sendMaxBytes,
		conn:          conn,
		netConn:       hijacker.conn,
		codec:         codec,
		info:          info,
		singleRequest: spec.StreamType&connect.StreamTypeClient == 0,
		messages:      make(chan websocketMessage),
	}
	if timeoutErr != nil {
		stream.finish(ctx, timeoutErr)
		return
	}
	go stream.read(opts.readMaxBytes) //nolint:contextcheck // the stream holds ctx
	metadata, err := stream.receiveMetadata()
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
	go stream.heartbeat() //nolint:contextcheck // the stream holds ctx
	fromHTTPHeader(info.RequestHeader(), header)
	stream.finish(ctx, scrubHandlerError(server.Call(ctx, spec.Procedure, info, stream)))
}

// heartbeat pings the client after websocketPingInterval without a write.
// coder closes the connection when a ping can't be written within 5s, which
// Receive reports as a client disconnect.
func (s *websocketServerStream) heartbeat() {
	timer := time.NewTimer(websocketPingInterval)
	defer timer.Stop()
	for {
		select {
		case <-s.ioCtx.Done():
			return
		case <-timer.C:
		}
		idle := time.Since(time.Unix(0, s.lastWriteUnixNano.Load()))
		if idle < websocketPingInterval {
			timer.Reset(websocketPingInterval - idle)
			continue
		}
		// Reset before Ping, which can wait a whole interval for the pong.
		timer.Reset(websocketPingInterval)
		// Return pong frames may not be read if the handler is not calling
		// Receive, or if the client is busy sending payloads. We ignore the
		// deadline since it does not mean the client disappeared.
		pingCtx, cancel := context.WithTimeout(s.ioCtx, websocketPingInterval)
		err := s.conn.Ping(pingCtx)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return
		}
	}
}

type websocketHijacker struct {
	http.ResponseWriter

	conn net.Conn
}

func (h *websocketHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, readWriter, err := http.NewResponseController(h.ResponseWriter).Hijack()
	h.conn = conn
	return conn, readWriter, err
}

// websocketRequestHeader validates the metadata and builds the RPC's
// effective headers to present to the handler. Metadata can't replace a
// handshake header, which a proxy may have set.
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

// websocketResponseMetadata builds M or S metadata. A client fails the call on
// invalid metadata but a server drops it, like a net/http server on HTTP/2. A
// connect- key is left out and fails the RPC, as spec Sections 7.4 and 9
// require.
func websocketResponseMetadata(header *connect.Header, kind string) (http.Header, error) {
	metadata := make(http.Header)
	var err error
	for key, values := range header.All() {
		if len(values) == 0 {
			continue
		}
		lowerKey := strings.ToLower(key)
		if strings.HasPrefix(lowerKey, "connect-") {
			if err == nil {
				err = connect.Errorf(connect.CodeInternal, "response %s %q is reserved for the protocol", kind, lowerKey)
			}
			continue
		}
		if _, ok := protocolHeaders[key]; ok || !websocketIsMetadataKey(lowerKey) {
			continue
		}
		for _, value := range values {
			value = websocketTrimMetadataValue(value)
			if strings.HasSuffix(lowerKey, "-bin") || websocketIsMetadataValue(value) {
				metadata[lowerKey] = append(metadata[lowerKey], value)
			}
		}
	}
	return metadata, err
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
	var timeoutValue string
	var found bool
	for pair := range strings.SplitSeq(rawQuery, "&") {
		key, value, _ := strings.Cut(pair, "=")
		if key, err := url.QueryUnescape(key); err != nil || key != websocketTimeoutQueryParameter {
			continue
		}
		value, err := url.QueryUnescape(value)
		if err != nil {
			return 0, false, connect.Errorf(connect.CodeInvalidArgument, "parse timeout: %s", err)
		}
		if found {
			return 0, false, connect.NewError(connect.CodeInvalidArgument, "parse timeout: more than one value supplied")
		}
		timeoutValue, found = value, true
	}
	if !found {
		return 0, false, nil
	} else if timeoutValue == "" {
		return 0, false, connect.NewError(connect.CodeInvalidArgument, "parse timeout: no value supplied")
	}
	timeout, err := connectParseTimeout(timeoutValue)
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
