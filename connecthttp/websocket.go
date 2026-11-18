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

// WebSocket composition. This package owns the entry points — Mount and
// NewTransport — and internal/wstransport owns the protocol, so the dependency
// runs one way and a caller configures both wires from one set of options.

package connecthttp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/wstransport"
)

// hybridTransport opens each stream on WebSocket or fallback according to the
// [Selector] in effect for its spec.
type hybridTransport struct {
	fallback   *transport
	options    *options
	httpClient *http.Client
	baseURL    string
	// base serves every spec when no conditional options are registered.
	base *webSocketRoute
	// routes caches each procedure's route when conditional options are
	// registered, since they may configure WebSocket differently per spec.
	routes sync.Map // procedure string -> *webSocketRoute
}

// webSocketRoute is the WebSocket configuration in effect for a spec.
type webSocketRoute struct {
	// transport is nil when selector is.
	transport       connect.Transport
	err             error
	selector        Selector
	fallbackOnError bool
}

func newWebSocketRoute(opts *options, httpClient *http.Client, baseURL string) *webSocketRoute {
	route := &webSocketRoute{
		selector:        opts.websocketSelector,
		fallbackOnError: opts.websocketFallbackOnErr,
	}
	if route.selector == nil {
		return route
	}
	clientOptions := opts.websocketClientOpts(httpClient)
	if opts.websocketFallbackOnErr {
		clientOptions = append(clientOptions, wstransport.WithEagerDial())
	}
	route.transport, route.err = wstransport.NewTransport(baseURL, clientOptions...)
	return route
}

func (t *hybridTransport) route(spec connect.Spec) *webSocketRoute {
	if len(t.options.conditional) == 0 {
		return t.base
	}
	if cached, ok := t.routes.Load(spec.Procedure); ok {
		if route, ok := cached.(*webSocketRoute); ok {
			return route
		}
	}
	route := newWebSocketRoute(t.fallback.clientStateForSpec(spec).opts, t.httpClient, t.baseURL)
	actual, _ := t.routes.LoadOrStore(spec.Procedure, route)
	if stored, ok := actual.(*webSocketRoute); ok {
		return stored
	}
	return route
}

func (t *hybridTransport) NewClientStream(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
	route := t.route(spec)
	if route.selector == nil || !route.selector(spec) {
		return t.fallback.NewClientStream(ctx, spec)
	}
	if route.err != nil {
		return nil, route.err
	}
	ctx, info := ensureClientCallInfo(ctx)
	// websocketHTTPClient records the handshake here.
	info.TransportInfo = &ClientInfo{}
	stream, err := route.transport.NewClientStream(ctx, spec)
	if err == nil {
		return stream, nil
	}
	// A canceled call is the caller's doing, not a peer that lacks WebSocket
	// support, so retrying on the fallback would only obscure the cause.
	if !route.fallbackOnError || ctx.Err() != nil {
		return nil, err
	}
	stream, fallbackErr := t.fallback.NewClientStream(ctx, spec)
	if fallbackErr != nil {
		return nil, errors.Join(err, fallbackErr)
	}
	// The WebSocket error is dropped here. A fallback transport is typically
	// lazy, so it usually succeeds at this point and fails later from Send, by
	// which time only its own error is left to report.
	return stream, nil
}

// websocketServerOpts translates the settings both wires share, so a codec or
// limit set once applies whichever way an RPC travels. Options with no
// WebSocket meaning are not forwarded; WebSocket-only settings come from the
// WithWebSocket* options.
func (o *options) websocketServerOpts() []wstransport.ServerOption {
	forwarded := []wstransport.ServerOption{
		wstransport.WithCodecs(o.codecs...),
		wstransport.WithReadMaxBytes(o.readMaxBytes),
		wstransport.WithSendMaxBytes(o.sendMaxBytes),
	}
	// Only when set. Zero means "no minimum" to both packages, but their
	// defaults differ — this one leaves it unset, and forwarding that would
	// silently turn off the WebSocket half's own 512-byte floor and compress
	// every 3-byte control message.
	if o.compressMinBytes > 0 {
		forwarded = append(forwarded, wstransport.WithCompressMinBytes(o.compressMinBytes))
	}
	return append(forwarded, o.websocketServerOptions...)
}

// websocketClientOpts is the client-side twin.
func (o *options) websocketClientOpts(httpClient *http.Client) []wstransport.ClientOption {
	forwarded := []wstransport.ClientOption{
		wstransport.WithCodecs(o.codecs...),
		wstransport.WithReadMaxBytes(o.readMaxBytes),
		wstransport.WithSendMaxBytes(o.sendMaxBytes),
	}
	if o.compressMinBytes > 0 {
		forwarded = append(forwarded, wstransport.WithCompressMinBytes(o.compressMinBytes))
	}
	// The effective name, not the option: without WithSendCodec the first
	// registered codec is the default, as it is over HTTP.
	if name := o.getSendCodecName(); name != "" {
		forwarded = append(forwarded, wstransport.WithSendCodec(name))
	}
	forwarded = append(forwarded, wstransport.WithHTTPClient(httpClient))
	return append(forwarded, o.websocketClientOptions...)
}

// ensureClientCallInfo returns the CallInfo on ctx, attaching a new one if
// there is none, as the HTTP transport does.
func ensureClientCallInfo(ctx context.Context) (context.Context, *connect.CallInfo) {
	if info, ok := connect.CallInfoForClientContext(ctx); ok {
		return ctx, info
	}
	return connect.NewClientContext(ctx)
}

// websocketHTTPClient returns the [*http.Client] the handshake goes through,
// so that it travels the same client as HTTP RPCs do. A client that is not an
// *http.Client is adapted rather than replaced, and either way the handshake is
// recorded on the call's [ClientInfo].
func websocketHTTPClient(httpClient HTTPClient) *http.Client {
	standard, isStandard := httpClient.(*http.Client)
	if httpClient == nil || (isStandard && standard == nil) {
		standard, isStandard = http.DefaultClient, true
	}
	if !isStandard {
		return &http.Client{Transport: &handshakeRecorder{roundTrip: func(request *http.Request) (*http.Response, error) {
			response, err := httpClient.Do(request)
			// The adapter's own http.Client wraps the error again.
			if urlErr, ok := errors.AsType[*url.Error](err); ok {
				return response, urlErr.Err
			}
			return response, err
		}}}
	}
	recording := *standard
	next := standard.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	recording.Transport = &handshakeRecorder{roundTrip: next.RoundTrip}
	return &recording
}

// handshakeRecorder stores a WebSocket handshake on the [ClientInfo] of the
// call that made it, as the HTTP transport stores each request.
type handshakeRecorder struct {
	roundTrip func(*http.Request) (*http.Response, error)
}

func (r *handshakeRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.roundTrip(request)
	if info, ok := ClientInfoForContext(request.Context()); ok {
		info.request.Store(request)
		if response != nil {
			info.response.Store(response)
		}
	}
	return response, err
}

// withWebSocket serves handler's route over WebSocket too, configured by opts,
// so that per-procedure options reach both transports. A route whose options
// disable WebSocket refuses upgrades instead.
//
// Both transports share every path, because which one carries an RPC is the
// client's choice: a server serving only HTTP would reject a client that
// upgraded.
func withWebSocket(
	server *connect.Server,
	handler http.Handler,
	opts *options,
	extra ...wstransport.ServerOption,
) http.Handler {
	if opts.websocketDisabled {
		return refuseUpgrade(handler)
	}
	serverOptions := append(opts.websocketServerOpts(), extra...)
	serverOptions = append(serverOptions, wstransport.WithTransportInfo(func(request *http.Request) any {
		return &ServerInfo{request: request}
	}))
	return wstransport.Upgrade(server, handler, serverOptions...)
}

// refuseUpgrade answers a WebSocket upgrade with 400, as an endpoint
// restricted to Connect over HTTP must (RFC 008 §4.2). Left to the HTTP
// handler, the handshake's GET would get an unrelated status.
func refuseUpgrade(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if wstransport.IsUpgrade(request) {
			http.Error(
				responseWriter,
				"this endpoint serves Connect over HTTP only, not over WebSocket",
				http.StatusBadRequest,
			)
			return
		}
		handler.ServeHTTP(responseWriter, request)
	})
}

// brokenTransport reports a configuration error from every RPC, which is where
// a caller will actually see it.
type brokenTransport struct{ err error }

func (t *brokenTransport) NewClientStream(context.Context, connect.Spec) (connect.ClientStream, error) {
	return nil, t.err
}
