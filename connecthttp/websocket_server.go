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
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/coder/websocket"
)

const (
	// websocketFirstMessageTimeout bounds the wait for the client's M message,
	// so a client that upgrades and goes quiet doesn't hold the connection.
	websocketFirstMessageTimeout = 10 * time.Second
	// websocketCloseTimeout bounds the final M and S writes, which can't use
	// the RPC context once it has expired.
	websocketCloseTimeout = 5 * time.Second
)

// websocketServerStream is the [connect.ServerStream] for one RPC over
// WebSocket. Receive is called from the handler's read side. SendHeaders and
// Send are called from its write side, and finish once the handler returns.
type websocketServerStream struct {
	ctx           context.Context //nolint:containedctx // the stream is the RPC's lifetime
	conn          *websocket.Conn
	codec         connect.Codec
	info          *connect.CallInfo
	reader        *websocketServerReader
	singleRequest bool

	// Read side.
	receivedEnd    bool
	receivedSingle bool
	// Write side.
	sentHeader bool
}

func (s *websocketServerStream) Receive(msg any) error {
	if !s.singleRequest {
		return s.receive(msg)
	}
	// Unary and server-streaming RPCs take exactly one request.
	if s.receivedSingle {
		return io.EOF
	}
	s.receivedSingle = true
	return receiveExactlyOne(s.receive, msg, "request")
}

// SendHeaders and Send write with the RPC context, so a canceled handler
// isn't stuck behind a client that stopped reading. If the context ends
// during a write, coder drops the connection and no S is sent.
func (s *websocketServerStream) SendHeaders() error {
	if err := s.ctx.Err(); err != nil {
		return wrapIfContextError(err)
	}
	return s.writeHeader(s.ctx)
}

func (s *websocketServerStream) Send(msg any) error {
	if err := s.ctx.Err(); err != nil {
		return wrapIfContextError(err)
	}
	if err := s.writeHeader(s.ctx); err != nil {
		return err
	}
	if err := writeWebSocketBody(s.ctx, s.conn, s.codec, msg); err != nil {
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
		// A deadline or a client failure ends the RPC while it waits.
		if err := s.reader.failure(); err != nil {
			return err
		}
		return wrapIfContextError(s.ctx.Err())
	}
	switch message.marker {
	case websocketMarkerBody:
		if err := s.codec.UnmarshalRead(s.ctx, bytes.NewReader(message.payload), msg); err != nil {
			return connect.Errorf(connect.CodeInvalidArgument, "unmarshal message: %s", err).WithCause(err)
		}
		return nil
	case websocketMarkerClientEndStream:
		s.receivedEnd = true
		return io.EOF
	default:
		err := newWebSocketProtocolError("unexpected %c message from client", message.marker)
		s.reader.fail(err)
		return err
	}
}

// writeHeader writes the M message from the response headers set so far,
// once. Headers set after it are dropped.
func (s *websocketServerStream) writeHeader(ctx context.Context) error {
	if s.sentHeader {
		return nil
	}
	metadata := encodeWebSocketMetadata(websocketMetadata(s.info.ResponseHeader()))
	if err := writeWebSocketText(ctx, s.conn, websocketMarkerLeadingMetadata, metadata); err != nil {
		return s.writeError(err)
	}
	s.sentHeader = true
	return nil
}

func (s *websocketServerStream) writeError(err error) error {
	err = wrapIfContextDone(s.ctx, err)
	if _, ok := asError(err); ok {
		return err
	}
	return connect.Errorf(connect.CodeUnavailable, "write message: %s", err).WithCause(err)
}

// finish ends the RPC with err as its verdict. It sends M if it is unsent,
// then S, then closes. It sends nothing to a client that has gone away.
func (s *websocketServerStream) finish(ctx context.Context, err error) {
	peerGone, failure := s.reader.stop()
	switch {
	case peerGone:
		_ = s.conn.CloseNow()
		return
	case errors.Is(failure, errWebSocketInvalidUTF8):
		_ = s.conn.Close(websocket.StatusInvalidFramePayloadData, "")
		return
	}
	closeCode := websocket.StatusNormalClosure
	if errors.Is(failure, errWebSocketProtocol) {
		err = failure
		closeCode = websocket.StatusProtocolError
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), websocketCloseTimeout)
	defer cancel()
	if writeErr := s.writeHeader(ctx); writeErr != nil {
		_ = s.conn.CloseNow()
		return
	}
	end := &connectEndStreamMessage{Trailer: websocketMetadata(s.info.ResponseTrailer())}
	if err != nil {
		end.Error = newConnectWireError(err)
	}
	payload, marshalErr := json.Marshal(end)
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

// websocketServerReader owns every read from the connection. It hands
// messages to the stream until the client's C, then keeps reading to notice
// the client going away, which cancels the RPC.
type websocketServerReader struct {
	conn       *websocket.Conn
	bodyIsText bool
	cancel     context.CancelCauseFunc
	messages   chan websocketMessage
	done       chan struct{} // closed by stop

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
	var sawEnd bool
	for {
		// Never a context the RPC can cancel: coder drops the connection
		// without a close frame when a read context ends. finish closes the
		// connection, which ends the read.
		messageType, data, err := r.conn.Read(context.WithoutCancel(ctx))
		if err != nil {
			r.connectionEnded(err)
			return
		}
		if r.isStopped() {
			// S is sent, so client messages still in flight are discarded.
			continue
		}
		message, parseErr := parseWebSocketMessage(messageType, data, r.bodyIsText)
		if parseErr != nil {
			r.fail(parseErr)
			return
		}
		if sawEnd {
			r.fail(newWebSocketProtocolError("%c message after C", message.marker))
			return
		}
		sawEnd = message.marker == websocketMarkerClientEndStream
		select {
		case r.messages <- message:
		case <-r.done:
		}
	}
}

// fail records err as the RPC's failure and cancels it. The first failure
// wins.
func (r *websocketServerReader) fail(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
	r.cancel(err)
}

// connectionEnded records the end of the connection. Before S it means the
// client canceled the RPC, or sent a message over the read limit and coder
// closed with 1009.
func (r *websocketServerReader) connectionEnded(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isStopped() {
		return
	}
	r.peerGone = true
	if r.err == nil {
		r.err = connect.Errorf(connect.CodeCanceled, "client closed the connection: %s", err).WithCause(err)
		if errors.Is(err, websocket.ErrMessageTooBig) {
			r.err = connect.Errorf(connect.CodeResourceExhausted, "read message: %s", err).WithCause(err)
		}
	}
	r.cancel(r.err)
}

// stop marks S as about to be sent and returns the failure so far. Later
// reads are discarded and the end of the connection is no longer a
// cancellation. It is called once.
func (r *websocketServerReader) stop() (peerGone bool, failure error) {
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

// newWebSocketHandler serves WebSocket upgrades for spec and passes every
// other request to next.
func newWebSocketHandler(server *connect.Server, spec connect.Spec, opts *options, next http.Handler) http.Handler {
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if !isWebSocketUpgrade(request) {
			next.ServeHTTP(responseWriter, request)
			return
		}
		serveWebSocket(server, spec, opts, responseWriter, request)
	})
}

// serveWebSocket serves one RPC over a WebSocket connection.
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
	// finish closes the connection. This covers a handler panic, because
	// net/http doesn't close hijacked connections.
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(websocketReadLimit(opts.readMaxBytes))

	ctx, cancel := context.WithCancelCause(request.Context())
	defer cancel(nil)
	timeout, hasTimeout, timeoutErr := parseWebSocketTimeout(request.URL.Query())
	if hasTimeout {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, timeout)
		defer cancelTimeout()
	}
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
	fromHTTPHeader(info.RequestHeader(), websocketRequestHeader(request, metadata))
	stream.finish(ctx, scrubHandlerError(server.Call(ctx, spec.Procedure, info, stream)))
}

// receiveWebSocketMetadata waits for the client's M message and decodes it.
func receiveWebSocketMetadata(ctx context.Context, reader *websocketServerReader) (http.Header, error) {
	// A zero timeout has expired already. Report it rather than racing it
	// against a message that has arrived.
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
		if message.marker != websocketMarkerLeadingMetadata {
			err := newWebSocketProtocolError("first message is %c, expected M", message.marker)
			reader.fail(err)
			return nil, err
		}
		metadata, err := decodeWebSocketMetadata(message.payload, true)
		if err != nil {
			reader.fail(err)
			return nil, err
		}
		return metadata, nil
	}
}

// websocketRequestHeader returns the RPC's effective request headers: the
// handshake headers without the ones the handshake itself uses, then each M
// entry replacing the handshake value.
func websocketRequestHeader(request *http.Request, metadata http.Header) http.Header {
	header := make(http.Header, len(request.Header)+len(metadata)+1)
	for key, values := range request.Header {
		if key == "Connection" || key == "Upgrade" ||
			strings.HasPrefix(key, "Sec-Websocket-") || strings.HasPrefix(key, "Connect-") {
			continue
		}
		header[key] = values
	}
	header.Set(headerHost, request.Host)
	maps.Copy(header, metadata)
	return header
}

// negotiateWebSocketCodec returns the codec and token for the first offered
// subprotocol that names a registered codec.
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

// parseWebSocketTimeout parses the connect-timeout-ms query parameter.
func parseWebSocketTimeout(query url.Values) (time.Duration, bool, error) {
	values, ok := query[websocketTimeoutQueryParameter]
	switch {
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

// isWebSocketUpgrade reports whether request asks to upgrade to WebSocket.
func isWebSocketUpgrade(request *http.Request) bool {
	return request.Method == http.MethodGet &&
		headerHasToken(request.Header, "Connection", "upgrade") &&
		headerHasToken(request.Header, "Upgrade", "websocket")
}

// headerHasToken reports whether the comma-separated header key contains
// token, compared case-insensitively.
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
