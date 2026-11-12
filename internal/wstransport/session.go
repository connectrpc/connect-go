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
	"time"

	"connectrpc.com/connect/v2"
	"github.com/coder/websocket"
)

// NewSession returns the [Session] that serves Connect RPCs on an upgraded
// connection. One connection carries one RPC: the procedure is the request
// path, so the upgrade itself selects the method.
//
// [Upgrade], [Mux], and [Mount] build this themselves. Call it only to wrap
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
	// Bounds both sides of permessage-deflate: the inflated message is capped at
	// the limit, and compressed input at roughly the limit plus a read-ahead
	// buffer. A stream of zero-output DEFLATE blocks, which never inflates to
	// anything, is still cut off at about the limit.
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
	ctx, cancel, timeoutErr := timeoutFromRequest(ctx, info.Request, info.MaxTimeout)
	if cancel != nil {
		defer cancel()
	}
	// Cancelable in its own right, so that the post-C drain can end the call
	// when the connection dies under it (§8.1). timeoutFromRequest returns no
	// cancel of its own when neither side set a deadline.
	ctx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	procedure := info.Request.URL.Path
	spec, registered := registeredSpec(server, procedure)
	if !registered {
		// Server.Call reports the unknown procedure; until then, bidi is the
		// permissive answer.
		spec = connect.Spec{Procedure: procedure, StreamType: connect.StreamTypeBidi}
	}
	callInfo := &connect.CallInfo{
		Spec:     spec,
		PeerAddr: info.PeerAddr,
		Protocol: ProtocolConnectWebSocket,
		Codec:    info.Codec.Name(),
		// Identity at the Connect layer: the transport's permessage-deflate is
		// below it and invisible to the protocol.
		RequestEncoding:  connect.CompressionNameIdentity,
		ResponseEncoding: connect.CompressionNameIdentity,
	}
	for key, values := range info.Request.Header {
		if isStrippedHandshakeHeader(key) {
			continue
		}
		callInfo.RequestHeader().SetValues(key, values)
	}

	// Detached from the RPC's deadline: see websocketHandlerConn.Close. Bounded
	// so that an unresponsive peer cannot hold the session open.
	closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), wsCloseWriteTimeout)
	defer closeCancel()
	// Every return below has closed conn by the time these run, which is what
	// lets the reader's read in flight return.
	//
	// The reader is detached from the RPC's deadline: the WebSocket library
	// closes the connection when a read's context expires, which would leave
	// no connection to send the deadline's S message on.
	readLimit := messageReadLimit(info.ReadMaxBytes)
	reader := newFrameReader(context.WithoutCancel(ctx), func(readCtx context.Context) (websocket.MessageType, []byte, error) {
		return readBoundedMessage(readCtx, conn, readLimit)
	})
	defer reader.close()
	keeper := startKeepAlive(
		ctx, conn, reader, info.KeepAliveInterval, info.KeepAliveTimeout, websocket.StatusInternalError,
	)
	defer keeper.stop()
	handlerConn := &websocketHandlerConn{
		request: info.Request,
		wsConn:  conn,
		drain:   info.drain,
		marshaler: messageWriter{
			// No compression here: the WebSocket transport compresses whole
			// messages itself, so a second pass would deflate gzip.
			ctx:          ctx,
			sender:       &websocketBinarySender{ctx: ctx, conn: conn},
			codecs:       info.Codecs,
			bodyIsText:   info.Codec.Name() == connect.CodecNameJSON,
			sendMaxBytes: info.SendMaxBytes,
			stats:        &callInfo.SendStats,
		},
		unmarshaler: websocketUnmarshaler{
			ctx:                   ctx,
			reader:                reader,
			keeper:                keeper,
			callInfo:              callInfo,
			codecs:                info.Codecs,
			bodyIsText:            info.Codec.Name() == connect.CodecNameJSON,
			cancelCall:            cancelCall,
			readMaxBytes:          info.ReadMaxBytes,
			infrastructureHeaders: info.InfrastructureHeaders,
			info:                  info,
		},
		callInfo:        callInfo,
		responseTrailer: make(http.Header),
		closeCtx:        closeCtx,
	}

	if timeoutErr != nil {
		// The handshake already committed the HTTP response, so a malformed
		// deadline can only be reported in the end-of-stream message.
		return handlerConn.Close(timeoutErr)
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
	if drainErr := handlerConn.unmarshaler.drainLeadingMetadata(); drainErr != nil {
		return handlerConn.Close(drainErr)
	}

	stream := &serverStream{
		conn:          handlerConn,
		singleRequest: spec.StreamType&connect.StreamTypeClient == 0,
	}
	callErr := server.Call(ctx, procedure, callInfo, stream)
	// Trailing metadata is only knowable once the handler has returned. Leading
	// metadata is read live by flushLeadingMetadata, which runs earlier.
	copyToHTTPHeader(handlerConn.responseTrailer, callInfo.ResponseTrailer())
	return handlerConn.Close(callErr)
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

// SendHeaders does nothing on the wire. The HTTP response was committed by the
// upgrade, so response metadata can only travel in the trailing
// EndStreamMessage, which Close writes.
func (s *serverStream) SendHeaders() error {
	return nil
}

func (s *serverStream) Send(msg any) error {
	return s.conn.Send(msg)
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
	values, present := request.URL.Query()[wsQueryTimeoutMs]
	if !present {
		// No client deadline, so the server's bound is the whole of it.
		if maxTimeout <= 0 {
			return ctx, nil, nil
		}
		ctx, cancel := context.WithTimeout(ctx, maxTimeout)
		return ctx, cancel, nil
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
	requested := time.Duration(millis) * time.Millisecond
	// A client may ask for less time than the server allows, never more.
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
