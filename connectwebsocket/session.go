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

package connectwebsocket

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/connectwire"
	"connectrpc.com/connect/v2/internal/envelope"
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
	// The bound only holds because a misbehaving peer is hung up on rather than
	// closed politely: the closing handshake drains whatever the peer queued.
	// See peerMisbehaved, and TestCompressionBombIsBoundedOnTheWire, which
	// fails if either half regresses.
	conn.SetReadLimit(frameReadLimit(info.ReadMaxBytes))
	ctx, cancel, timeoutErr := timeoutFromRequest(ctx, info.Request)
	if cancel != nil {
		defer cancel()
	}
	procedure := info.Request.URL.Path
	callInfo := &connect.CallInfo{
		Spec: connect.Spec{
			Procedure: procedure,
			// The real spec reaches the handler as an argument; the registry
			// lookup in Server.Call has not happened yet, so the stream shape is
			// unknown here and bidi is the permissive answer.
			StreamType: connect.StreamTypeBidi,
		},
		PeerAddr: info.PeerAddr,
		Protocol: ProtocolConnectWebSocket,
		Codec:    info.Codec.Name(),
		// Identity at the Connect layer: the transport's permessage-deflate is
		// below it and invisible to the protocol.
		RequestEncoding:  connect.CompressionNameIdentity,
		ResponseEncoding: connect.CompressionNameIdentity,
	}
	for key, values := range info.Request.Header {
		callInfo.RequestHeader().SetValues(key, values)
	}

	// Detached from the RPC's deadline: see websocketHandlerConn.Close. Bounded
	// so that an unresponsive peer cannot hold the session open.
	closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), wsCloseWriteTimeout)
	defer closeCancel()
	handlerConn := &websocketHandlerConn{
		request: info.Request,
		wsConn:  conn,
		marshaler: connectwire.StreamingMarshaler{
			Writer: envelope.Writer{
				Ctx:    ctx,
				Sender: &websocketBinarySender{ctx: ctx, conn: conn},
				Codec:  info.Codec,
				// No CompressionPool: the WebSocket transport compresses whole
				// messages itself, so a second pass here would deflate gzip.
				SendMaxBytes: info.SendMaxBytes,
				Stats:        &callInfo.SendStats,
			},
		},
		unmarshaler: websocketUnmarshaler{
			ctx:          ctx,
			wsConn:       conn,
			callInfo:     callInfo,
			codec:        info.Codec,
			readMaxBytes: info.ReadMaxBytes,
			info:         info,
		},
		callInfo:        callInfo,
		responseTrailer: make(http.Header),
		closeCtx:        closeCtx,
	}

	if timeoutErr != nil {
		// The handshake already committed the HTTP response, so a malformed
		// deadline can only be reported in the end-of-stream envelope.
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

	callErr := server.Call(ctx, procedure, callInfo, &serverStream{conn: handlerConn})
	// Trailing metadata is only knowable once the handler has returned. Leading
	// metadata is read live by flushLeadingMetadata, which runs earlier.
	copyToHTTPHeader(handlerConn.responseTrailer, callInfo.ResponseTrailer())
	return handlerConn.Close(callErr)
}

// serverStream adapts [websocketHandlerConn] to [connect.ServerStream].
type serverStream struct {
	conn *websocketHandlerConn
}

func (s *serverStream) Receive(msg any) error {
	return s.conn.Receive(msg)
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
// The deadline arrives as a header, or as a query parameter for browsers,
// whose WebSocket API cannot set headers on the upgrade request. Both are
// accepted, and they must agree.
func timeoutFromRequest(
	ctx context.Context,
	request *http.Request,
) (context.Context, context.CancelFunc, *connect.Error) {
	headerValue := request.Header.Get(headerTimeout)
	queryValue := request.URL.Query().Get(wsQueryTimeoutMs)
	var timeout string
	switch {
	case headerValue != "" && queryValue != "" && headerValue != queryValue:
		return ctx, nil, errorf(
			connect.CodeInvalidArgument,
			"conflicting %s header (%q) and %s query parameter (%q)",
			headerTimeout, headerValue, wsQueryTimeoutMs, queryValue,
		)
	case headerValue != "":
		timeout = headerValue
	case queryValue != "":
		timeout = queryValue
	default:
		return ctx, nil, nil
	}
	if len(timeout) > 10 {
		return ctx, nil, errorf(connect.CodeInvalidArgument, "parse timeout: %q has >10 digits", timeout)
	}
	millis, err := strconv.ParseInt(timeout, 10 /* base */, 64 /* bitSize */)
	if err != nil {
		return ctx, nil, errorf(connect.CodeInvalidArgument, "parse timeout: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(millis)*time.Millisecond)
	return ctx, cancel, nil
}

func copyToHTTPHeader(dst http.Header, src *connect.Header) {
	for key, values := range src.All() {
		dst[http.CanonicalHeaderKey(key)] = append(dst[http.CanonicalHeaderKey(key)], values...)
	}
}
