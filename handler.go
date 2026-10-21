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

package connect

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	connectv2 "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
)

// A Handler is the server-side implementation of a single RPC defined by a
// service schema.
//
// By default, Handlers support the Connect, gRPC, and gRPC-Web protocols with
// the binary Protobuf and JSON codecs. They support gzip compression using the
// standard library's [compress/gzip].
type Handler struct {
	spec      Spec
	handler   http.Handler
	procedure string
}

// NewUnaryHandler constructs a [Handler] for a request-response procedure.
func NewUnaryHandler[Req, Res any](
	procedure string,
	unary func(context.Context, *Request[Req]) (*Response[Res], error),
	options ...HandlerOption,
) *Handler {
	cfg := newHandlerConfig(procedure, StreamTypeUnary, options)
	spec := cfg.newSpec()

	typed := UnaryFunc(func(ctx context.Context, req AnyRequest) (AnyResponse, error) {
		typedReq, ok := req.(*Request[Req])
		if !ok {
			return nil, errorf(CodeInternal, "unexpected handler request type %T", req)
		}
		res, err := unary(ctx, typedReq)
		if res == nil && err == nil {
			panic(procedure + " returned nil *connect.Response and nil error") //nolint:forbidigo
		}
		if res == nil {
			return nil, err
		}
		return res, err
	})
	if interceptor := cfg.chainInterceptor(); interceptor != nil {
		typed = interceptor.WrapUnary(typed)
	}

	server := connectv2.NewServer()
	server.Register(connectv2.Method{
		Spec: spec.toConnectv2(),
		Handler: func(ctx context.Context, _ connectv2.Spec, stream connectv2.ServerStream) error {
			v2Info := serverCallInfo(ctx)
			peer := serverPeerFromContext(ctx)
			info := &handlerCallInfo{
				spec:          spec,
				peer:          peer,
				method:        serverHTTPMethod(ctx),
				requestHeader: handlerRequestHeader(ctx, v2Info, spec.StreamType),
			}
			ctx, err := runRequestGates(ctx, cfg, spec, peer, info.requestHeader)
			if err != nil {
				mergeMetadataFromError(v2Info, err, isGRPCFamilyProtocol(ctx))
				return v1ErrorToConnectv2(err)
			}
			msg := new(Req)
			if err := cfg.initializer.maybe(spec, msg); err != nil {
				return err
			}
			if err := stream.Receive(msg); err != nil {
				return err
			}
			ctx = newHandlerContext(ctx, info)
			wrappedReq := &Request[Req]{
				Msg:    msg,
				spec:   spec,
				peer:   peer,
				header: info.requestHeader,
				method: info.method,
			}
			res, err := typed(ctx, wrappedReq)
			if err != nil {
				// Apply handler-set headers/trailers first; merging error
				// metadata after lets the error's keys append to handler
				// trailers rather than overwriting them.
				applyHandlerCallInfoOutbound(v2Info, info)
				// gRPC and gRPC-Web encode unary errors as trailers-only; the
				// Connect protocol carries error metadata in response headers.
				asTrailers := isGRPCFamilyProtocol(ctx)
				mergeMetadataFromError(v2Info, err, asTrailers)
				return v1ErrorToConnectv2(err)
			}
			if res != nil {
				mergeHeaders(info.ResponseHeader(), res.Header())
				mergeHeaders(info.ResponseTrailer(), res.Trailer())
			}
			applyHandlerCallInfoOutbound(v2Info, info)
			if res == nil {
				return nil
			}
			return stream.Send(res.Any())
		},
	})
	return newHandler(spec, server, cfg)
}

// NewUnaryHandlerSimple constructs a [Handler] for a request-response procedure using the
// function signature associated with the "simple" generation option.
//
// This option eliminates the [Request] and [Response] wrappers, and instead uses the
// context.Context to propagate information such as headers.
func NewUnaryHandlerSimple[Req, Res any](
	procedure string,
	unary func(context.Context, *Req) (*Res, error),
	options ...HandlerOption,
) *Handler {
	return NewUnaryHandler(
		procedure,
		func(ctx context.Context, request *Request[Req]) (*Response[Res], error) {
			responseMsg, err := unary(ctx, request.Msg)
			if err != nil {
				return nil, err
			}
			return NewResponse(responseMsg), nil
		},
		options...,
	)
}

// NewClientStreamHandler constructs a [Handler] for a client streaming procedure.
func NewClientStreamHandler[Req, Res any](
	procedure string,
	implementation func(context.Context, *ClientStream[Req]) (*Response[Res], error),
	options ...HandlerOption,
) *Handler {
	cfg := newHandlerConfig(procedure, StreamTypeClient, options)
	spec := cfg.newSpec()
	server := connectv2.NewServer()
	server.Register(connectv2.Method{
		Spec: spec.toConnectv2(),
		Handler: func(ctx context.Context, _ connectv2.Spec, v2stream connectv2.ServerStream) error {
			return runStreamingHandler(ctx, cfg, spec, v2stream, func(ctx context.Context, conn StreamingHandlerConn) error {
				stream := &ClientStream[Req]{
					conn:        conn,
					initializer: cfg.initializer,
				}
				res, err := implementation(ctx, stream)
				if err != nil {
					return err
				}
				if res == nil {
					panic(procedure + " returned nil *connect.Response and nil error") //nolint:forbidigo
				}
				mergeHeaders(conn.ResponseHeader(), res.header)
				mergeHeaders(conn.ResponseTrailer(), res.trailer)
				return conn.Send(res.Msg)
			})
		},
	})
	return newHandler(spec, server, cfg)
}

// NewClientStreamHandlerSimple constructs a [Handler] for a request-streaming procedure
// using the function signature associated with the "simple" generation option.
//
// This option eliminates the [Response] wrapper, and instead uses the context.Context
// to propagate information such as headers.
func NewClientStreamHandlerSimple[Req, Res any](
	procedure string,
	implementation func(context.Context, *ClientStream[Req]) (*Res, error),
	options ...HandlerOption,
) *Handler {
	return NewClientStreamHandler(
		procedure,
		func(ctx context.Context, stream *ClientStream[Req]) (*Response[Res], error) {
			responseMsg, err := implementation(ctx, stream)
			if err != nil {
				return nil, err
			}
			return NewResponse(responseMsg), nil
		},
		options...,
	)
}

// NewServerStreamHandler constructs a [Handler] for a server streaming procedure.
func NewServerStreamHandler[Req, Res any](
	procedure string,
	implementation func(context.Context, *Request[Req], *ServerStream[Res]) error,
	options ...HandlerOption,
) *Handler {
	cfg := newHandlerConfig(procedure, StreamTypeServer, options)
	spec := cfg.newSpec()
	server := connectv2.NewServer()
	server.Register(connectv2.Method{
		Spec: spec.toConnectv2(),
		Handler: func(ctx context.Context, _ connectv2.Spec, v2stream connectv2.ServerStream) error {
			return runStreamingHandler(ctx, cfg, spec, v2stream, func(ctx context.Context, conn StreamingHandlerConn) error {
				req, err := receiveUnaryHandlerRequest[Req](ctx, conn, cfg.initializer)
				if err != nil {
					return err
				}
				return implementation(ctx, req, &ServerStream[Res]{conn: conn})
			})
		},
	})
	return newHandler(spec, server, cfg)
}

// NewServerStreamHandlerSimple constructs a [Handler] a server streaming procedure using the function
// signature associated with the "simple" generation option.
//
// This option eliminates the [Request] wrapper, and instead uses the context.Context to
// propagate information such as headers.
func NewServerStreamHandlerSimple[Req, Res any](
	procedure string,
	implementation func(context.Context, *Req, *ServerStream[Res]) error,
	options ...HandlerOption,
) *Handler {
	return NewServerStreamHandler(
		procedure,
		func(ctx context.Context, request *Request[Req], serverStream *ServerStream[Res]) error {
			return implementation(ctx, request.Msg, serverStream)
		},
		options...,
	)
}

// NewBidiStreamHandler constructs a [Handler] for a bidirectional streaming procedure.
func NewBidiStreamHandler[Req, Res any](
	procedure string,
	implementation func(context.Context, *BidiStream[Req, Res]) error,
	options ...HandlerOption,
) *Handler {
	cfg := newHandlerConfig(procedure, StreamTypeBidi, options)
	spec := cfg.newSpec()
	server := connectv2.NewServer()
	server.Register(connectv2.Method{
		Spec: spec.toConnectv2(),
		Handler: func(ctx context.Context, _ connectv2.Spec, v2stream connectv2.ServerStream) error {
			return runStreamingHandler(ctx, cfg, spec, v2stream, func(ctx context.Context, conn StreamingHandlerConn) error {
				return implementation(ctx, &BidiStream[Req, Res]{
					conn:        conn,
					initializer: cfg.initializer,
				})
			})
		},
	})
	return newHandler(spec, server, cfg)
}

// newHandler wires the v2 server through connecthttp.Mount and returns the
// resulting [Handler].
func newHandler(spec Spec, server *connectv2.Server, cfg *handlerConfig) *Handler {
	mux := &procedureMux{procedure: spec.Procedure}
	connecthttp.Mount(mux, server, handlerMountOptions(cfg)...)
	return &Handler{
		spec:      spec,
		handler:   mux.handler,
		procedure: spec.Procedure,
	}
}

// procedureMux captures the handler [connecthttp.Mount] registers for a
// single procedure. Like v1, the resulting [Handler] serves every request it
// receives, whatever the URL path.
type procedureMux struct {
	procedure string
	handler   http.Handler
}

func (m *procedureMux) Handle(pattern string, handler http.Handler) {
	if pattern == m.procedure {
		m.handler = handler
	}
}

// ServeHTTP implements [http.Handler].
//
// Bidirectional streaming RPCs require HTTP/2. When a bidi handler receives
// an HTTP/1.x request, the handler closes the underlying TCP connection and
// returns 505 HTTP Version Not Supported, matching v1's behavior. This check
// runs before delegating to the v2 mux so HTTP/1.1 clients see a clear
// failure mode instead of a hung connection.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if (h.spec.StreamType&StreamTypeBidi) == StreamTypeBidi && request.ProtoMajor < 2 {
		writer.Header().Set("Connection", "close")
		writer.WriteHeader(http.StatusHTTPVersionNotSupported)
		return
	}
	h.handler.ServeHTTP(writer, request)
}

// runStreamingHandler builds a v1 [StreamingHandlerConn] from a v2 server
// stream and invokes inner (which is the user's typed stream handler, run
// inside any registered interceptor chain).
func runStreamingHandler(
	ctx context.Context,
	cfg *handlerConfig,
	spec Spec,
	v2stream connectv2.ServerStream,
	inner func(context.Context, StreamingHandlerConn) error,
) error {
	v2Info := serverCallInfo(ctx)
	peer := serverPeerFromContext(ctx)
	conn := &handlerStreamConn{
		spec:       spec,
		peer:       peer,
		stream:     v2stream,
		v2Info:     v2Info,
		reqHeader:  handlerRequestHeader(ctx, v2Info, spec.StreamType),
		resHeader:  make(http.Header),
		resTrailer: make(http.Header),
	}
	streamFunc := func(ctx context.Context, c StreamingHandlerConn) error {
		ctx = newHandlerContext(ctx, &streamingHandlerCallInfo{conn: c})
		return inner(ctx, c)
	}
	if interceptor := cfg.chainInterceptor(); interceptor != nil {
		streamFunc = interceptor.WrapStreamingHandler(streamFunc)
	}
	ctx, err := runRequestGates(ctx, cfg, spec, peer, conn.reqHeader)
	if err == nil {
		err = streamFunc(ctx, conn)
	}
	// On exit, propagate accumulated response/trailer metadata to the v2
	// CallInfo so the transport encodes them on the wire.
	conn.flushMetadata()
	if err != nil {
		mergeMetadataFromError(v2Info, err, true)
	}
	return v1ErrorToConnectv2(err)
}

// handlerMountOptions translates handlerConfig fields into the connecthttp
// option set used to Mount the v2 server.
func handlerMountOptions(cfg *handlerConfig) []connecthttp.Option {
	opts := []connecthttp.Option{
		connecthttp.WithCodecs(assembleHandlerCodecs(cfg)...),
		connecthttp.WithCompressors(compressorsToConnectv2(cfg.compressorOrder, cfg.compressors)...),
		connecthttp.WithCompressMinBytes(cfg.compressMinBytes),
		connecthttp.WithReadMaxBytes(cfg.readMaxBytes),
		connecthttp.WithSendMaxBytes(cfg.sendMaxBytes),
	}
	if cfg.requireConnectProtocol {
		opts = append(opts, connecthttp.WithRequireConnectProtocolHeader())
	}
	return opts
}

func assembleHandlerCodecs(cfg *handlerConfig) []connectv2.Codec {
	return assembleCodecs(
		[]connectv2.Codec{connectproto.NewBinaryCodec(), connectproto.NewJSONCodec()},
		cfg.codecOverrides,
	)
}

// runRequestGates runs the handler's [RequestGateFunc]s in order. Each gate
// sees the context returned by the previous one.
func runRequestGates(ctx context.Context, cfg *handlerConfig, spec Spec, peer Peer, header http.Header) (context.Context, error) {
	for _, gate := range cfg.requestGates {
		gateCtx, err := gate(ctx, spec, peer, header)
		if err != nil {
			return ctx, err
		}
		if gateCtx != nil {
			ctx = gateCtx //nolint:fatcontext // Chaining is intended: each gate sees the previous gate's context.
		}
	}
	return ctx, nil
}

// serverCallInfo returns the v2 server CallInfo for ctx, or an empty one when
// the handler runs outside a v2 server (e.g. unit tests).
func serverCallInfo(ctx context.Context) *connectv2.CallInfo {
	if info, ok := connectv2.CallInfoForServerContext(ctx); ok {
		return info
	}
	return &connectv2.CallInfo{}
}

// serverPeerFromContext derives a v1 [Peer] for the current handler context
// from the v2 CallInfo and connecthttp ServerInfo.
func serverPeerFromContext(ctx context.Context) Peer {
	callInfo, ok := connectv2.CallInfoForServerContext(ctx)
	if !ok {
		return Peer{}
	}
	peer := Peer{
		Addr:     callInfo.PeerAddr,
		Protocol: callInfo.Protocol,
	}
	if info, ok := connecthttp.ServerInfoForContext(ctx); ok {
		u := info.RequestURL()
		peer.Query = u.Query()
	}
	return peer
}

// serverHTTPMethod returns the HTTP method for the current handler context,
// or POST as the safe default when the connecthttp info is not attached
// (e.g. unit tests that exercise the [UnaryFunc] directly).
func serverHTTPMethod(ctx context.Context) string {
	if info, ok := connecthttp.ServerInfoForContext(ctx); ok {
		if m := info.HTTPMethod(); m != "" {
			return m
		}
	}
	return http.MethodPost
}

// handlerRequestHeader builds the v1 inbound request header. v2's
// transport-agnostic CallInfo.RequestHeader carries only user metadata, but
// connect-go v1 exposes the raw inbound header via Request.Header(). The shim
// restores the protocol-reserved headers v1 callers expect via
// rebuildProtocolHeaders.
func handlerRequestHeader(ctx context.Context, v2Info *connectv2.CallInfo, streamType StreamType) http.Header {
	header := metadataToHTTPHeader(v2Info.RequestHeader())
	rebuildProtocolHeaders(ctx, header, streamType)
	return header
}

// rebuildProtocolHeaders restores the protocol-reserved request headers that
// connect-go v1 exposes through Request.Header() but v2 keeps off its
// transport-agnostic CallInfo. It reconstructs them from the connecthttp
// transport's negotiated protocol, codec, request compression, and the request
// deadline:
//
//   - Content-Type (always, when a codec is resolved).
//   - The request compression header: Grpc-Encoding for gRPC / gRPC-Web,
//     Content-Encoding for Connect unary, Connect-Content-Encoding for Connect
//     streaming. Omitted for identity.
//   - The timeout header: Grpc-Timeout for gRPC / gRPC-Web, Connect-Timeout-Ms
//     for Connect. v2 consumes the inbound timeout into the context deadline, so
//     the reconstructed value reflects the deadline remaining when the handler
//     runs rather than the exact value the client sent.
//
// Content-Length is not restored: net/http derives it from the body and the
// transport does not surface it as metadata. It is a no-op when the connecthttp
// info or codec is unavailable (e.g. unit tests exercising a [UnaryFunc]
// directly).
func rebuildProtocolHeaders(ctx context.Context, header http.Header, streamType StreamType) {
	info, ok := connectv2.CallInfoForServerContext(ctx)
	if !ok {
		return
	}
	protocol := info.Protocol
	if contentType := requestContentType(protocol, info.Codec, streamType); contentType != "" {
		header.Set("Content-Type", contentType)
	}
	if encoding := info.RequestEncoding; encoding != "" && encoding != connectv2.CompressionNameIdentity {
		switch protocol {
		case connectv2.ProtocolNameGRPC, connectv2.ProtocolNameGRPCWeb:
			header.Set("Grpc-Encoding", encoding)
		case connectv2.ProtocolNameConnect:
			if streamType == StreamTypeUnary {
				header.Set("Content-Encoding", encoding)
			} else {
				header.Set("Connect-Content-Encoding", encoding)
			}
		}
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 {
			switch protocol {
			case connectv2.ProtocolNameGRPC, connectv2.ProtocolNameGRPCWeb:
				header.Set("Grpc-Timeout", grpcEncodeTimeout(remaining))
			case connectv2.ProtocolNameConnect:
				header.Set("Connect-Timeout-Ms", strconv.FormatInt(remaining.Milliseconds(), 10))
			}
		}
	}
}

// requestContentType reconstructs the inbound Content-Type from the negotiated
// protocol and codec, mirroring the value the client sent on the wire. It
// returns the empty string when the codec is unavailable.
func requestContentType(protocol, codec string, streamType StreamType) string {
	if codec == "" {
		return ""
	}
	switch protocol {
	case connectv2.ProtocolNameGRPC:
		return "application/grpc+" + codec
	case connectv2.ProtocolNameGRPCWeb:
		return "application/grpc-web+" + codec
	case connectv2.ProtocolNameConnect:
		if streamType == StreamTypeUnary {
			return "application/" + codec
		}
		return "application/connect+" + codec
	}
	return ""
}

// grpcEncodeTimeout formats d as a gRPC-Timeout header value: up to eight digits
// followed by a unit. It selects the smallest unit whose value fits in eight
// digits, rounding up so the reconstructed timeout never under-reports.
func grpcEncodeTimeout(duration time.Duration) string {
	if duration <= 0 {
		return "0n"
	}
	for _, unit := range []struct {
		size time.Duration
		char string
	}{
		{time.Nanosecond, "n"},
		{time.Microsecond, "u"},
		{time.Millisecond, "m"},
		{time.Second, "S"},
		{time.Minute, "M"},
		{time.Hour, "H"},
	} {
		if value := (duration + unit.size - 1) / unit.size; value < 1e8 {
			return strconv.FormatInt(int64(value), 10) + unit.char
		}
	}
	return "99999999H"
}

// receiveUnaryHandlerRequest receives one request message and wraps it in a
// v1 [*Request] for server-streaming handlers.
func receiveUnaryHandlerRequest[Req any](_ context.Context, conn StreamingHandlerConn, init maybeInitializer) (*Request[Req], error) {
	var msg Req
	if err := init.maybe(conn.Spec(), &msg); err != nil {
		return nil, err
	}
	if err := conn.Receive(&msg); err != nil {
		return nil, err
	}
	return &Request[Req]{
		Msg:    &msg,
		spec:   conn.Spec(),
		peer:   conn.Peer(),
		header: conn.RequestHeader(),
		method: http.MethodPost,
	}, nil
}

// applyHandlerCallInfoOutbound projects the v1 handler's accumulated response
// header / trailer state into the v2 CallInfo so the transport can encode
// them on the wire.
func applyHandlerCallInfoOutbound(v2Info *connectv2.CallInfo, info *handlerCallInfo) {
	if v2Info == nil {
		return
	}
	if info.responseHeader != nil {
		copyHTTPHeaderIntoMetadata(v2Info.ResponseHeader(), info.responseHeader)
	}
	if info.responseTrailer != nil {
		copyHTTPHeaderIntoMetadata(v2Info.ResponseTrailer(), info.responseTrailer)
	}
}

// connectUnaryTrailerPrefix is the Connect unary on-the-wire marker that
// distinguishes a trailer from a header: user trailer "X" travels as the
// response header "Trailer-X".
const connectUnaryTrailerPrefix = "Trailer-"

// mergeMetadataFromError merges any v1 *Error.Meta() into the v2 response
// metadata so error headers reach the wire. v1 strips meta from wire-tagged
// errors at the encode boundary so handlers can't accidentally forward a
// downstream peer's headers to their own caller.
//
// asTrailers picks the right side of the call info: streaming errors land
// in trailers (the end-stream envelope, gRPC trailers, gRPC-Web trailer
// frame), while unary errors land in the response headers.
//
// HTTP-protocol headers (Content-Type, Content-Length, etc.) are dropped
// to match v1's mergeNonProtocolHeaders behavior, since handlers may
// not use error.Meta() to overwrite transport-controlled headers.
func mergeMetadataFromError(v2Info *connectv2.CallInfo, err error, asTrailers bool) {
	if v2Info == nil || err == nil {
		return
	}
	verr, ok := asError(err)
	if !ok {
		return
	}
	if verr.wireErr {
		return
	}
	if len(verr.meta) == 0 {
		return
	}
	for key, vals := range verr.meta {
		if len(vals) == 0 {
			continue
		}
		if isProtocolReservedHeader(key) {
			continue
		}
		dst := v2Info.ResponseHeader()
		name := key
		switch {
		case asTrailers:
			// gRPC and gRPC-Web encode all unary error metadata as trailers.
			dst = v2Info.ResponseTrailer()
		case strings.HasPrefix(key, connectUnaryTrailerPrefix):
			// Connect unary carries trailers as "Trailer-"-prefixed entries in
			// the error metadata. Route them to the trailer side (de-prefixed)
			// so the transport re-encodes them; otherwise v2 drops them as
			// stray "Trailer-*" response headers.
			dst = v2Info.ResponseTrailer()
			name = key[len(connectUnaryTrailerPrefix):]
		}
		for _, v := range vals {
			dst.Add(name, v)
		}
	}
}

// isGRPCFamilyProtocol reports whether the inbound RPC uses gRPC or
// gRPC-Web. Both encode unary errors as trailers-only responses, so v1
// must route error metadata into the trailer side of the v2 CallInfo
// rather than the response headers. Connect unary instead carries error
// metadata in response headers.
func isGRPCFamilyProtocol(ctx context.Context) bool {
	info, ok := connectv2.CallInfoForServerContext(ctx)
	if !ok {
		return false
	}
	switch info.Protocol {
	case connectv2.ProtocolNameGRPC, connectv2.ProtocolNameGRPCWeb:
		return true
	}
	return false
}

// isProtocolReservedHeader reports whether key is reserved by HTTP or by
// the Connect/gRPC wire protocols. v1 historically blocked these from
// flowing through error.Meta(). The set mirrors v2's protocolHeaders but
// stays local so this package doesn't depend on connecthttp internals.
func isProtocolReservedHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Content-Type",
		"Content-Length",
		"Content-Encoding",
		"Accept-Encoding",
		"Host",
		"Trailer",
		"Date",
		"Connect-Protocol-Version",
		"Connect-Timeout-Ms",
		"Connect-Content-Encoding",
		"Connect-Accept-Encoding",
		"Grpc-Encoding",
		"Grpc-Accept-Encoding",
		"Grpc-Timeout",
		"Grpc-Status",
		"Grpc-Message",
		"Grpc-Status-Details-Bin":
		return true
	}
	return false
}

// v1ErrorOnly translates an error returned by the v2 runtime into v1's
// expected error shape. A v1 [*Error] is passed through unchanged; a v2
// [*connectv2.Error] is unwrapped into a v1 [*Error]; raw context errors get
// the matching v1 code; anything else is returned as-is.
func v1ErrorOnly(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := asError(err); ok {
		return err
	}
	if v1err := errorFromConnectv2(err); v1err != nil {
		return v1err
	}
	return wrapIfContextError(err)
}

// v1ErrorToConnectv2 prepares err for return from a v1 handler into the v2
// runtime. v1 errors with the v1 NotModified sentinel become v2's
// connecthttp.NewNotModifiedError() so v2's GET handler emits a 304. Other v1
// *Error values are translated via Error.toConnectv2 so codes, messages, and
// details survive. Non-Error values pass through unchanged.
func v1ErrorToConnectv2(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errNotModified) {
		return connecthttp.NewNotModifiedError()
	}
	v1err, ok := asError(err)
	if !ok {
		return err
	}
	return v1err.toConnectv2()
}
