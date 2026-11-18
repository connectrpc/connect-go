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

package wstransport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/coder/websocket"
)

// NewSession returns the [Session] that serves Connect RPCs on an upgraded
// connection. One connection carries one RPC: the procedure is the one given
// to [WithProcedure], or else the request path.
//
// [Upgrade] builds this itself. Call it only to wrap
// the default behavior in a [Session] of your own and pass the result to
// [WithSession]; it needs no configuring, because everything it uses arrives
// on [SessionInfo].
func NewSession() Session {
	return &session{}
}

// session holds no configuration: everything it needs arrives on SessionInfo,
// so a Session cannot be configured differently from the handshake that
// produced it.
type session struct{}

func (s *session) Serve(
	ctx context.Context,
	server *connect.Server,
	conn *websocket.Conn,
	info SessionInfo,
) error {
	// Bounds both sides of permessage-deflate (§12): the inflated message is
	// capped at the limit, and its compressed input at twice the limit plus a
	// margin, which drainLimiter counts on the connection. A stream of
	// zero-output DEFLATE blocks, which never inflates to anything, is cut off
	// there.
	//
	// The bound only holds because the closing handshake's drain is capped
	// too: it would otherwise read whatever the peer queued. See drainLimiter,
	// and TestCompressionBombIsBoundedOnTheWire, which fails if either half
	// regresses.
	//
	// The limit is enforced in readBoundedMessage rather than here: coder's own
	// SetReadLimit closes with 1009 the moment it trips, leaving nothing to
	// write the S message on.
	conn.SetReadLimit(-1)
	serveCtx := ctx
	ctx, cancel, timeoutErr := timeoutFromRequest(ctx, info.Request, info.MaxTimeout)
	if cancel != nil {
		defer cancel()
	}
	// Cancelable in its own right, so that the reader can end the call when
	// the connection dies under it (§8). timeoutFromRequest returns no cancel
	// of its own when neither side set a deadline.
	ctx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	procedure, spec := sessionSpec(server, info)
	callInfo := newCallInfo(spec, info)

	// Every return below has closed conn by the time these run, which is what
	// lets the reader's read in flight return.
	//
	// The reader is detached from the RPC's deadline: the WebSocket library
	// closes the connection when a read's context expires, which would leave
	// no connection to send the deadline's S message on.
	//
	// §8: a connection that ends before S cancels the RPC, whether or not C
	// arrived and whether or not the handler is reading. Once S is sent the
	// handler has returned, so the cancellation reaches nobody.
	readLimit := messageReadLimit(info.ReadMaxBytes)
	reader := newFrameReader(
		context.WithoutCancel(ctx),
		func(readCtx context.Context) (websocket.MessageType, []byte, error) {
			return readBoundedMessage(readCtx, conn, readLimit, info.drain)
		},
		cancelCall,
	)
	info.heartbeat.attach(reader)
	defer reader.close()
	// Detached, as the reader is: the library closes the connection when a
	// Ping's context ends, so the RPC's end would cut off its own S.
	keeper := startKeepAlive(
		context.WithoutCancel(ctx), conn, reader, info.KeepAliveInterval, info.KeepAliveTimeout,
		websocket.StatusInternalError,
	)
	defer keeper.stop()
	handlerConn := &websocketHandlerConn{
		ctx:    ctx,
		wsConn: conn,
		marshaler: messageWriter{
			// No compression here: the WebSocket transport compresses whole
			// messages itself, so a second pass would deflate gzip.
			sender:       &websocketBinarySender{conn: conn},
			sendMaxBytes: info.SendMaxBytes,
			stats:        &callInfo.SendStats,
		},
		unmarshaler: websocketUnmarshaler{
			frameDecoder: frameDecoder{
				readMaxBytes: info.ReadMaxBytes,
			},
			reader:     reader,
			keeper:     keeper,
			callInfo:   callInfo,
			cancelCall: cancelCall,
			info:       info,
		},
		shuttingDown: func() bool { return isShuttingDown(serveCtx) },
	}

	if timeoutErr != nil {
		// The handshake already committed the HTTP response, so a malformed
		// deadline can only be reported in the end-of-stream message.
		return handlerConn.Close(ctx, timeoutErr)
	}

	// net/http recovers a panicking handler but deliberately leaves a hijacked
	// connection open ("if !c.hijacked()"), so unlike an HTTP handler nothing
	// closes the socket on the way out: the server leaks it and the client
	// blocks on a Receive that will never return. Tear it down, then let the
	// panic continue to whatever recovers it — converting panics into RPC
	// errors is an interceptor's job, not the transport's.
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = conn.CloseNow()
			panic(recovered)
		}
	}()

	// Consume the client's Leading-Metadata before the handler exists, so
	// CallInfo is complete for interceptors and frozen for the handler. See
	// drainLeadingMetadata.
	if drainErr := handlerConn.unmarshaler.drainLeadingMetadata(ctx); drainErr != nil {
		return handlerConn.Close(ctx, drainErr)
	}

	stream := &serverStream{
		conn:          handlerConn,
		singleRequest: spec.StreamType&connect.StreamTypeClient == 0,
	}
	callErr := server.Call(ctx, procedure, callInfo, stream)
	return handlerConn.Close(ctx, callErr)
}

// sessionSpec names the procedure a session calls and finds its spec.
func sessionSpec(server *connect.Server, info SessionInfo) (string, connect.Spec) {
	procedure := info.Procedure
	if procedure == "" {
		procedure = info.Request.URL.Path
	}
	spec, registered := registeredSpec(server, procedure)
	if !registered {
		// Server.Call reports the unknown procedure; until then, bidi is the
		// permissive answer.
		spec = connect.Spec{Procedure: procedure, StreamType: connect.StreamTypeBidi}
	}
	return procedure, spec
}

// newCallInfo describes the RPC to the handler, starting from the upgrade
// request's headers. The client's M message adds to them later.
func newCallInfo(spec connect.Spec, info SessionInfo) *connect.CallInfo {
	callInfo := &connect.CallInfo{
		Spec:     spec,
		PeerAddr: info.Request.RemoteAddr,
		Protocol: ProtocolConnectWebSocket,
		Codec:    info.Codec.Name(),
		// Identity at the Connect layer: the transport's permessage-deflate is
		// below it and invisible to the protocol.
		RequestEncoding:  connect.CompressionNameIdentity,
		ResponseEncoding: connect.CompressionNameIdentity,
		TransportInfo:    info.TransportInfo,
	}
	for key, values := range info.Request.Header {
		if isStrippedHandshakeHeader(key) {
			continue
		}
		callInfo.RequestHeader().SetValues(key, values)
	}
	// net/http moves Host out of the header map; over HTTP, connecthttp puts
	// it back, and a handler should not have to know which transport ran it.
	if info.Request.Host != "" {
		callInfo.RequestHeader().Set("Host", info.Request.Host)
	}
	return callInfo
}

// registeredSpec finds the spec server registered for procedure.
func registeredSpec(server *connect.Server, procedure string) (connect.Spec, bool) {
	for spec := range server.Specs() {
		if spec.Procedure == procedure {
			return spec, true
		}
	}
	return connect.Spec{}, false
}

// serverStream adapts [websocketHandlerConn] to [connect.ServerStream].
type serverStream struct {
	conn *websocketHandlerConn
	// singleRequest marks a unary or server-streaming call, whose one request
	// Receive checks is followed by C, as connecthttp does.
	singleRequest bool
	received      bool
}

func (s *serverStream) Receive(msg any) error {
	if !s.singleRequest {
		return s.conn.Receive(msg)
	}
	if s.received {
		return io.EOF
	}
	s.received = true
	if err := s.conn.Receive(msg); err != nil {
		if errors.Is(err, io.EOF) {
			return connect.Errorf(connect.CodeUnimplemented, "unary request has zero messages")
		}
		return err
	}
	// Reading on to C is also what starts the post-C reads that §8.1 requires,
	// since the handler never reads again.
	if err := s.conn.Receive(msg); err == nil {
		return connect.Errorf(connect.CodeUnimplemented, "unary request has multiple messages")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// SendHeaders sends the M message now, before any body (§7.4). Headers set
// after it are late and are not sent.
func (s *serverStream) SendHeaders() error {
	if err := s.conn.flushLeadingMetadata(s.conn.ctx); err != nil {
		return err
	}
	return nil // literal nil; a nil *connect.Error is a non-nil error
}

func (s *serverStream) Send(msg any) error {
	return s.conn.Send(msg)
}

// rawQueryValues returns every value of name in a raw query string. Unlike
// URL.Query, it keeps a value that is not valid percent-encoding, or that a
// semicolon runs into, rather than dropping the pair: §11 makes such a
// deadline invalid, not absent.
func rawQueryValues(rawQuery, name string) []string {
	var values []string
	for pair := range strings.SplitSeq(rawQuery, "&") {
		key, value, _ := strings.Cut(pair, "=")
		if unescaped, err := url.QueryUnescape(key); err == nil {
			key = unescaped
		}
		if key != name {
			continue
		}
		if unescaped, err := url.QueryUnescape(value); err == nil {
			value = unescaped
		}
		values = append(values, value)
	}
	return values
}

// timeoutFromRequest applies the client's deadline to ctx.
//
// The query parameter is the only channel: a browser's WebSocket API cannot
// set a header on the upgrade, so a header would serve no client this
// transport has to interoperate with. A Connect-Timeout-Ms header that arrives
// anyway is ordinary request metadata, not a deadline.
func timeoutFromRequest(
	ctx context.Context,
	request *http.Request,
	maxTimeout time.Duration,
) (context.Context, context.CancelFunc, *connect.Error) {
	values := rawQueryValues(request.URL.RawQuery, wsQueryTimeoutMs)
	if len(values) == 0 {
		// No client deadline, so the server's bound is the whole of it.
		if maxTimeout <= 0 {
			return ctx, nil, nil
		}
		ctx, cancel := context.WithTimeout(ctx, maxTimeout)
		return ctx, cancel, nil
	}
	// §11: a deadline stated twice is invalid, rather than a choice between
	// the two that the client cannot see the server make.
	if len(values) > 1 {
		return ctx, nil, errorf(
			connect.CodeInvalidArgument, "parse timeout: %s appears %d times", wsQueryTimeoutMs, len(values),
		)
	}
	timeout := values[0]
	if timeout == "" {
		return ctx, nil, errorf(connect.CodeInvalidArgument, "parse timeout: no value supplied")
	}
	if len(timeout) > 10 {
		return ctx, nil, errorf(connect.CodeInvalidArgument, "parse timeout: %q has >10 digits", timeout)
	}
	// §11 allows digits alone, as Connect over HTTP does: no sign, so neither
	// a negative deadline nor "+" gets through.
	var millis int64
	for _, digit := range timeout {
		if digit < '0' || digit > '9' {
			return ctx, nil, errorf(
				connect.CodeInvalidArgument, "parse timeout: %q has non-digit characters", timeout,
			)
		}
		millis = millis*10 + int64(digit-'0')
	}
	// §11: as over HTTP, a zero timeout means the RPC is not dispatched at all.
	if millis == 0 {
		return ctx, nil, errorf(
			connect.CodeDeadlineExceeded, "%s is 0, so the deadline has already passed", wsQueryTimeoutMs,
		)
	}
	requested := time.Duration(millis) * time.Millisecond
	// The effective deadline is the shorter of the client's and the server's.
	if maxTimeout > 0 && requested > maxTimeout {
		requested = maxTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, requested)
	return ctx, cancel, nil
}

func copyToHTTPHeader(dst http.Header, src *connect.Header) {
	for key, values := range src.All() {
		dst[http.CanonicalHeaderKey(key)] = append(dst[http.CanonicalHeaderKey(key)], values...)
	}
}
