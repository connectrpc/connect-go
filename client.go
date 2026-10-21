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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strings"

	connectv2 "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
)

// Client is a reusable, concurrency-safe client for a single procedure.
// Depending on the procedure's type, use the CallUnary, CallClientStream,
// CallServerStream, or CallBidiStream method.
//
// By default, clients use the Connect protocol with the binary Protobuf Codec,
// ask for gzipped responses, and send uncompressed requests. To use the gRPC
// or gRPC-Web protocols, use the [WithGRPC] or [WithGRPCWeb] options.
type Client[Req, Res any] struct {
	cfg       *clientConfig
	transport connectv2.Transport
	baseURL   *url.URL
	err       error // set when construction fails; returned from all calls
}

// NewClient constructs a new Client.
func NewClient[Req, Res any](httpClient HTTPClient, rawURL string, options ...ClientOption) *Client[Req, Res] {
	parsed, err := parseRequestURL(rawURL)
	if err != nil {
		return &Client[Req, Res]{err: err}
	}
	procedure := extractProtoPath(parsed.Path)
	cfg := newClientConfig(procedure, options)
	if err := cfg.validate(); err != nil {
		return &Client[Req, Res]{err: err}
	}
	// v2 joins the transport's base URL with the procedure. Like v1, send to
	// the exact URL given: an empty base path joined with the full path.
	baseURL := *parsed
	baseURL.Path, baseURL.RawPath = "", ""
	transport := newTransport(httpClient, baseURL.String(), cfg)
	return &Client[Req, Res]{
		cfg:       cfg,
		transport: transport,
		baseURL:   parsed,
	}
}

// CallUnary calls a request-response procedure.
func (c *Client[Req, Res]) CallUnary(ctx context.Context, request *Request[Req]) (*Response[Res], error) {
	if c.err != nil {
		return nil, c.err
	}
	spec := c.cfg.newSpec(StreamTypeUnary)
	// Bind CallInfo + sentinel before the interceptor chain so each thunk
	// can detect an interceptor that smuggled in a fresh context.
	ctx = installSentinel(ctx)
	response := NewResponse(new(Res))
	unary := func(ctx context.Context, req AnyRequest) (AnyResponse, error) {
		typed, ok := req.(*Request[Req])
		if !ok {
			return nil, errorf(CodeInternal, "unexpected client request type %T", req)
		}
		// Open a v2 stream so we can read out HTTP method, peer, response
		// metadata, etc.
		ctx, info := ensureClientInfo(ctx)
		info.spec = spec
		info.peer = c.peer()
		// Merge request wrapper headers and v1 CallInfo headers into the v2
		// metadata before dispatch. Both v1 entry points are valid sources
		// of request headers.
		mergeMetadataWithHTTPHeader(info.v2Info.RequestHeader(), info.requestHeader)
		mergeMetadataWithHTTPHeader(info.v2Info.RequestHeader(), typed.Header())
		applyClientDefaultHeaders(info.v2Info.RequestHeader(), c.cfg.protocol, info.requestHeader, typed.Header())
		v2stream, err := c.transport.NewClientStream(ctx, c.v2Spec(spec))
		if err != nil {
			return nil, c.finishClientErr(typed, info, err)
		}
		defer v2stream.Close()
		// Bind the v2-level CallInfo so RequestHeader on the response is
		// observable to interceptors after dispatch.
		bindClientHTTPInfo(ctx, info)
		if err := v2stream.Send(typed.Msg); err != nil {
			return nil, c.finishClientErr(typed, info, err)
		}
		if err := v2stream.CloseSend(); err != nil {
			return nil, c.finishClientErr(typed, info, err)
		}
		if err := c.cfg.initializer.maybe(spec, response.Msg); err != nil {
			return nil, err
		}
		if err := v2stream.Receive(response.Msg); err != nil {
			return nil, c.finishClientErr(typed, info, err)
		}
		// Surface request method (POST/GET) onto the v1 request wrapper.
		typed.setRequestMethod(info.HTTPMethod())
		// Project v2 response metadata back onto the v1 response wrapper.
		response.header = metadataToHTTPHeader(info.v2Info.ResponseHeader())
		response.trailer = metadataToHTTPHeader(info.v2Info.ResponseTrailer())
		if response.header.Get("Content-Type") == "" {
			response.header.Set("Content-Type", c.clientContentType(false))
		}
		return response, nil
	}
	if interceptor := c.cfg.chainInterceptor(); interceptor != nil {
		unary = interceptor.WrapUnary(unary)
	}
	request.spec = spec
	request.peer = c.peer()
	// Pre-populate protocol headers on the v1 request wrapper so interceptors
	// can inspect Content-Type before the v2 transport dispatches. v2 sets
	// these headers on the raw http.Request, not on metadata.
	if request.Header().Get("Content-Type") == "" {
		request.Header().Set("Content-Type", c.clientContentType(false))
	}
	result, err := unary(ctx, request)
	if err != nil {
		return nil, err
	}
	typed, ok := result.(*Response[Res])
	if !ok {
		return nil, errorf(CodeInternal, "unexpected client response type %T", result)
	}
	return typed, nil
}

// CallClientStream calls a client streaming procedure.
//
// Request headers can be sent via the [ClientStreamForClient.RequestHeader] method on the stream. Note that the
// request headers are not sent automatically when this method is invoked and instead require an explicit call to
// [ClientStreamForClient.Send].
func (c *Client[Req, Res]) CallClientStream(ctx context.Context) *ClientStreamForClient[Req, Res] {
	if c.err != nil {
		return &ClientStreamForClient[Req, Res]{err: c.err}
	}
	return &ClientStreamForClient[Req, Res]{
		conn:        c.openStreamConn(ctx, StreamTypeClient),
		initializer: c.cfg.initializer,
	}
}

// CallClientStreamSimple calls a client streaming procedure.
//
// Request headers should be set in a [CallInfo] object inside the context using [NewClientContext]. These headers are
// transmitted when this method is called and do not require an explicit call to [ClientStreamForClientSimple.Send].
//
// In addition, when calling [ClientStreamForClientSimple.CloseAndReceive] on the returned stream, the returned response
// is the response type defined for the stream and _not_ a Connect [Response] wrapper type. As a result, any response
// headers and trailers should be read from the [CallInfo] object in context.
func (c *Client[Req, Res]) CallClientStreamSimple(ctx context.Context) (*ClientStreamForClientSimple[Req, Res], error) {
	if c.err != nil {
		return &ClientStreamForClientSimple[Req, Res]{
			stream: &ClientStreamForClient[Req, Res]{err: c.err},
		}, c.err
	}
	stream := &ClientStreamForClientSimple[Req, Res]{
		stream: &ClientStreamForClient[Req, Res]{
			conn:        c.openStreamConn(ctx, StreamTypeClient),
			initializer: c.cfg.initializer,
		},
	}
	if err := stream.Send(nil); err != nil {
		return nil, err
	}
	return stream, nil
}

// CallServerStream calls a server streaming procedure.
func (c *Client[Req, Res]) CallServerStream(ctx context.Context, request *Request[Req]) (*ServerStreamForClient[Res], error) {
	if c.err != nil {
		return nil, c.err
	}
	conn := c.openStreamConn(ctx, StreamTypeServer)
	request.spec = conn.Spec()
	request.peer = conn.Peer()
	request.setRequestMethod(http.MethodPost)
	mergeHeaders(conn.RequestHeader(), request.header)
	if err := conn.Send(request.Msg); err != nil && !errors.Is(err, io.EOF) {
		_ = conn.CloseRequest()
		_ = conn.CloseResponse()
		return nil, err
	}
	if err := conn.CloseRequest(); err != nil {
		return nil, err
	}
	return &ServerStreamForClient[Res]{
		conn:        conn,
		initializer: c.cfg.initializer,
	}, nil
}

// CallBidiStream calls a bidirectional streaming procedure.
//
// Request headers can be sent via the [BidiStreamForClient.RequestHeader] method. Note that the
// request headers are not sent automatically when this method is invoked and instead require an explicit call to
// [BidiStreamForClient.Send].
func (c *Client[Req, Res]) CallBidiStream(ctx context.Context) *BidiStreamForClient[Req, Res] {
	if c.err != nil {
		return &BidiStreamForClient[Req, Res]{err: c.err}
	}
	return &BidiStreamForClient[Req, Res]{
		conn:        c.openStreamConn(ctx, StreamTypeBidi),
		initializer: c.cfg.initializer,
	}
}

// CallBidiStreamSimple calls a bidirectional streaming procedure.
//
// Request headers should be set in a [CallInfo] object inside the context using [NewClientContext]. These headers
// are transmitted when this method is called and do not require an explicit call to [BidiStreamForClient.Send].
//
// Likewise, response headers and trailers should be read from the [CallInfo] object in context.
func (c *Client[Req, Res]) CallBidiStreamSimple(ctx context.Context) (*BidiStreamForClientSimple[Req, Res], error) {
	if c.err != nil {
		return &BidiStreamForClientSimple[Req, Res]{
			stream: &BidiStreamForClient[Req, Res]{err: c.err},
		}, c.err
	}
	stream := &BidiStreamForClientSimple[Req, Res]{
		stream: &BidiStreamForClient[Req, Res]{
			conn:        c.openStreamConn(ctx, StreamTypeBidi),
			initializer: c.cfg.initializer,
		},
	}
	if err := stream.Send(nil); err != nil {
		return nil, err
	}
	return stream, nil
}

// openStreamConn opens a v2 stream for streamType and wraps it into a v1
// [StreamingClientConn]. Any client-side interceptor chain wraps the returned
// conn before it reaches the caller.
func (c *Client[Req, Res]) openStreamConn(ctx context.Context, streamType StreamType) StreamingClientConn {
	spec := c.cfg.newSpec(streamType)
	ctx = installSentinel(ctx)
	newConn := func(ctx context.Context, _ Spec) StreamingClientConn {
		ctx, info := ensureClientInfo(ctx)
		info.spec = spec
		info.peer = c.peer()
		v2Spec := c.v2Spec(spec)
		// Pre-populate Content-Type on the v1 request header so streaming
		// client interceptors can observe it. v2 sets Content-Type on the
		// raw HTTP request rather than CallInfo metadata.
		if info.RequestHeader().Get("Content-Type") == "" {
			info.RequestHeader().Set("Content-Type", c.clientContentType(true))
		}
		// Open the v2 stream eagerly so the conn can flow through the
		// interceptor chain. v2 normally dispatches lazily on first
		// Start/Send/Receive, but the v2 transport flushes request headers
		// on the first Send anyway.
		v2stream, err := c.transport.NewClientStream(ctx, v2Spec)
		if err != nil {
			return newErrClientConn(spec, c.peer(), v1ErrorOnly(err))
		}
		bindClientHTTPInfo(ctx, info)
		return &clientStreamConn{
			ctx:      ctx,
			protocol: c.cfg.protocol,
			spec:     spec,
			peer:     c.peer(),
			stream:   v2stream,
			info:     info,
		}
	}
	if interceptor := c.cfg.chainInterceptor(); interceptor != nil {
		newConn = interceptor.WrapStreamingClient(newConn)
	}
	return newConn(ctx, spec)
}

// v2Spec returns spec for the v2 transport, with the request path as the
// procedure so the request goes to the client's URL.
func (c *Client[Req, Res]) v2Spec(spec Spec) connectv2.Spec {
	v2Spec := spec.toConnectv2()
	v2Spec.Procedure = c.baseURL.Path
	return v2Spec
}

func (c *Client[Req, Res]) peer() Peer {
	if c.baseURL == nil {
		return Peer{}
	}
	protocol := c.cfg.protocol
	if protocol == "" {
		protocol = ProtocolConnect
	}
	return newPeerForURL(c.baseURL, protocol)
}

// clientContentType returns the Content-Type the v1 client expects from the
// server for its configured protocol and send codec. v2 strips Content-Type
// from response metadata as a protocol header, so v1 reconstructs it on the
// response wrapper to match historical behavior.
func (c *Client[Req, Res]) clientContentType(streaming bool) string {
	codec := codecNameProto
	if c.cfg.sendCodec != nil {
		codec = c.cfg.sendCodec.Name()
	}
	switch c.cfg.protocol {
	case "grpc":
		return "application/grpc"
	case "grpcweb":
		return "application/grpc-web+" + codec
	default:
		if streaming {
			return "application/connect+" + codec
		}
		return "application/" + codec
	}
}

var (
	// defaultConnectUserAgent returns a User-Agent string similar to those used in gRPC.
	//
	//nolint:gochecknoglobals
	defaultConnectUserAgent = fmt.Sprintf("connect-go/%s (%s)", Version, runtime.Version())
	// defaultGrpcUserAgent follows
	// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#user-agents.
	//
	//nolint:gochecknoglobals
	defaultGrpcUserAgent = fmt.Sprintf("grpc-go-connect/%s (%s)", Version, runtime.Version())
)

// applyClientDefaultHeaders sets v1's default User-Agent and, for gRPC-Web,
// X-User-Agent on the v2 request metadata. Like v1, a User-Agent key present
// in any of the caller's headers suppresses the default, even with no values.
func applyClientDefaultHeaders(metadata *connectv2.Header, protocol string, sources ...http.Header) {
	if metadata == nil {
		return
	}
	if !metadata.Has("User-Agent") {
		userAgent := defaultConnectUserAgent
		if protocol == ProtocolGRPC || protocol == ProtocolGRPCWeb {
			userAgent = defaultGrpcUserAgent
		}
		for _, source := range sources {
			if _, ok := source["User-Agent"]; ok {
				// An explicit empty value tells net/http to send no
				// User-Agent at all.
				userAgent = ""
				break
			}
		}
		metadata.Set("User-Agent", userAgent)
	}
	if protocol == ProtocolGRPCWeb && metadata.Get("X-User-Agent") == "" {
		metadata.Set("X-User-Agent", defaultGrpcUserAgent)
	}
}

// finishClientErr converts a v2 error to a v1 *Error and projects per-call
// state (HTTP method, response metadata, trailer metadata) onto the request
// wrapper and the error's Meta. v1 tests rely on observing response headers
// from an *Error (e.g. the ETag on a 304 Not Modified). Always returns a
// non-nil error when err is non-nil.
func (c *Client[Req, Res]) finishClientErr(req *Request[Req], info *clientCallInfo, err error) error {
	v1err := errorFromConnectv2(err)
	if v1err == nil {
		return err
	}
	if info == nil {
		return v1err
	}
	if method := info.HTTPMethod(); method != "" && req != nil {
		req.setRequestMethod(method)
	}
	mergeCallInfoMetaIntoError(info.v2Info, v1err)
	// Surface v1's NotModified sentinel when the HTTP transport observed a
	// 304 response. v2's not-modified error uses a private sentinel that
	// v1's IsNotModifiedError cannot see.
	if info.httpInfo != nil && info.httpInfo.ResponseStatus() == http.StatusNotModified {
		v1err.err = errNotModifiedClient
	}
	return v1err
}

// mergeCallInfoMetaIntoError attaches response and trailer metadata from the
// v2 CallInfo onto the v1 error's Meta. Tests depend on observing response
// headers from an [*Error] (e.g. the ETag on a 304 Not Modified).
func mergeCallInfoMetaIntoError(v2Info *connectv2.CallInfo, v1err *Error) {
	if v2Info == nil {
		return
	}
	meta := metadataToHTTPHeader(v2Info.ResponseHeader())
	// A gRPC trailers-only response carries Grpc-Status in the headers. v2
	// also copies those headers into the trailers, so like v1, use only the
	// headers to avoid duplicating every value.
	if _, trailersOnly := meta["Grpc-Status"]; !trailersOnly {
		mergeHeaders(meta, metadataToHTTPHeader(v2Info.ResponseTrailer()))
	}
	if len(meta) == 0 {
		return
	}
	if v1err.meta == nil {
		v1err.meta = meta
		return
	}
	mergeHeaders(v1err.meta, meta)
}

// parseRequestURL mirrors v1's URL parsing behavior, including the friendlier
// error message for grpc-style URLs missing a scheme.
func parseRequestURL(rawURL string) (*url.URL, *Error) {
	parsed, err := url.ParseRequestURI(rawURL)
	if err == nil {
		return parsed, nil
	}
	if !strings.Contains(rawURL, "://") {
		err = fmt.Errorf(
			"URL %q missing scheme: use http:// or https:// (unlike grpc-go)",
			rawURL,
		)
	}
	return nil, NewError(CodeUnavailable, err)
}

// newTransport builds a connecthttp transport from the v1 client config,
// translating v1 options into their connecthttp equivalents.
func newTransport(httpClient HTTPClient, baseURL string, cfg *clientConfig) connectv2.Transport {
	return connecthttp.NewTransport(httpClient, baseURL, transportOptionsFromClient(cfg)...)
}

// transportOptionsFromClient translates the v1 client config into connecthttp
// options. Every setting is passed explicitly, including v1's defaults, so a
// change to v2's defaults can't change v1's behavior.
func transportOptionsFromClient(cfg *clientConfig) []connecthttp.Option {
	opts := []connecthttp.Option{
		connecthttp.WithCodecs(assembleClientCodecs(cfg)...),
		connecthttp.WithCompressors(compressorsToConnectv2(cfg.compressorOrder, cfg.compressors)...),
		connecthttp.WithCompressMinBytes(cfg.compressMinBytes),
		connecthttp.WithReadMaxBytes(cfg.readMaxBytes),
		connecthttp.WithSendMaxBytes(cfg.sendMaxBytes),
	}
	if cfg.sendCodec != nil {
		opts = append(opts, connecthttp.WithSendCodec(cfg.sendCodec.Name()))
	}
	if cfg.sendCompressionName != "" && cfg.sendCompressionName != compressionIdentity {
		opts = append(opts, connecthttp.WithSendCompression(cfg.sendCompressionName))
	}
	if cfg.enableGet {
		opts = append(
			opts,
			connecthttp.WithHTTPGet(),
			connecthttp.WithHTTPGetMaxURLSize(cfg.getURLMaxBytes, cfg.getUseFallback),
		)
	}
	switch cfg.protocol {
	case ProtocolGRPC:
		opts = append(opts, connecthttp.WithGRPC())
	case ProtocolGRPCWeb:
		opts = append(opts, connecthttp.WithGRPCWeb())
	}
	return opts
}

// assembleClientCodecs returns the codecs to install on the v2 transport for
// this client. The default codec is binary protobuf; v1 explicitly ships JSON
// only when requested via [WithProtoJSON]. User overrides via [WithCodec]
// replace the defaults by name.
func assembleClientCodecs(cfg *clientConfig) []connectv2.Codec {
	return assembleCodecs([]connectv2.Codec{connectproto.NewBinaryCodec()}, cfg.codecOverrides)
}

// assembleCodecs returns defaults followed by overrides, in a stable order.
// An override replaces a default or earlier override with the same name.
func assembleCodecs(defaults []connectv2.Codec, overrides []Codec) []connectv2.Codec {
	codecs := defaults
	for _, override := range overrides {
		codec := codecToConnectv2(override)
		index := slices.IndexFunc(codecs, func(c connectv2.Codec) bool {
			return c.Name() == codec.Name()
		})
		if index >= 0 {
			codecs[index] = codec
			continue
		}
		codecs = append(codecs, codec)
	}
	return codecs
}

// compressorsToConnectv2 adapts the registered v1 compressors. v1 prefers the
// last registered compressor while v2 prefers the first listed, so the order
// is reversed.
func compressorsToConnectv2(order []string, entries map[string]*compressorEntry) []connectv2.Compressor {
	compressors := make([]connectv2.Compressor, 0, len(order))
	for _, name := range slices.Backward(order) {
		if entry := entries[name]; entry != nil {
			compressors = append(compressors, compressorToConnectv2(name, entry.newDecompressor, entry.newCompressor))
		}
	}
	return compressors
}

// ensureClientInfo guarantees that ctx carries a v1 [clientCallInfo] and a v2
// CallInfo, returning the augmented context and the v1 info.
func ensureClientInfo(ctx context.Context) (context.Context, *clientCallInfo) {
	info, ok := clientCallInfoForContext(ctx)
	if !ok {
		ctx, _ = NewClientContext(ctx)
		info, _ = clientCallInfoForContext(ctx)
	}
	// v2Info holds per-RPC request, response, and trailer metadata. A v1
	// NewClientContext may be reused across sequential calls (see the doc on
	// NewClientContext), so mint a fresh v2Info every dispatch - reusing it
	// would append this call's request headers onto the previous call's
	// metadata and surface the previous call's stale response metadata.
	newCtx, v2 := connectv2.NewClientContext(ctx)
	info.v2Info = v2
	info.responseSource = v2ResponseSource{info: v2}
	return newCtx, info
}

// bindClientHTTPInfo attaches the connecthttp ClientInfo on ctx (if any) to
// info so v1 callers can observe HTTPMethod, peer addr, etc.
func bindClientHTTPInfo(ctx context.Context, info *clientCallInfo) {
	if info == nil {
		return
	}
	if hi, ok := connecthttp.ClientInfoForContext(ctx); ok {
		info.httpInfo = hi
		info.method = hi.HTTPMethod()
	}
}

// errClientConn is a sentinel StreamingClientConn returned when stream
// construction fails. All methods return the bound error.
type errClientConn struct {
	spec            Spec
	peer            Peer
	err             error
	requestHeader   http.Header
	responseHeader  http.Header
	responseTrailer http.Header
}

func newErrClientConn(spec Spec, peer Peer, err error) *errClientConn {
	return &errClientConn{
		spec:            spec,
		peer:            peer,
		err:             err,
		requestHeader:   make(http.Header),
		responseHeader:  make(http.Header),
		responseTrailer: make(http.Header),
	}
}

func (c *errClientConn) Spec() Spec                   { return c.spec }
func (c *errClientConn) Peer() Peer                   { return c.peer }
func (c *errClientConn) Send(any) error               { return c.err }
func (c *errClientConn) RequestHeader() http.Header   { return c.requestHeader }
func (c *errClientConn) CloseRequest() error          { return c.err }
func (c *errClientConn) Receive(any) error            { return c.err }
func (c *errClientConn) ResponseHeader() http.Header  { return c.responseHeader }
func (c *errClientConn) ResponseTrailer() http.Header { return c.responseTrailer }
func (c *errClientConn) CloseResponse() error         { return c.err }

// installSentinel records the client call info (creating one if necessary)
// under [sentinelContextKey], so [checkSentinel] detects interceptors that
// replace the client context.
func installSentinel(ctx context.Context) context.Context {
	if _, ok := clientCallInfoForContext(ctx); !ok {
		ctx, _ = NewClientContext(ctx)
	}
	info, _ := clientCallInfoForContext(ctx)
	return context.WithValue(ctx, sentinelContextKey{}, info)
}
