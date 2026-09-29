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
// NewTransport — and connectwebsocket owns the protocol, so the dependency
// runs one way and a caller configures both wires from one set of options.

package connecthttp

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/wstransport"
)

// hybridTransport opens each stream on websocket or fallback according to a
// [Selector].
type hybridTransport struct {
	websocket       connect.Transport
	fallback        connect.Transport
	selector        Selector
	fallbackOnError bool
}

func (t *hybridTransport) NewClientStream(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
	if t.websocket == nil || !t.selector(spec) {
		return t.fallback.NewClientStream(ctx, spec)
	}
	stream, err := t.websocket.NewClientStream(ctx, spec)
	if err == nil {
		return stream, nil
	}
	// A canceled call is the caller's doing, not a peer that lacks WebSocket
	// support, so retrying on the fallback would only obscure the cause.
	if !t.fallbackOnError || ctx.Err() != nil {
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

// websocketServerOptions translates the settings both wires share, so a codec
// or limit set once applies whichever way an RPC travels. Options with no
// WebSocket meaning are not forwarded; WebSocket-only settings are supplied
// through WithWebSocketOptions.
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
	if o.websocketPrefix != "" {
		forwarded = append(forwarded, wstransport.WithPathPrefix(o.websocketPrefix))
	}
	return append(forwarded, o.websocketServerOptions...)
}

// websocketClientOpts is the client-side twin.
func (o *options) websocketClientOpts(httpClient HTTPClient) []wstransport.ClientOption {
	forwarded := []wstransport.ClientOption{
		wstransport.WithCodecs(o.codecs...),
		wstransport.WithReadMaxBytes(o.readMaxBytes),
		wstransport.WithSendMaxBytes(o.sendMaxBytes),
	}
	if o.compressMinBytes > 0 {
		forwarded = append(forwarded, wstransport.WithCompressMinBytes(o.compressMinBytes))
	}
	if o.sendCodecName != "" {
		forwarded = append(forwarded, wstransport.WithSendCodec(o.sendCodecName))
	}
	if o.websocketPrefix != "" {
		forwarded = append(forwarded, wstransport.WithPathPrefix(o.websocketPrefix))
	}
	if standard, ok := httpClient.(*http.Client); ok && standard != nil {
		forwarded = append(forwarded, wstransport.WithHTTPClient(standard))
	}
	return append(forwarded, o.websocketClientOptions...)
}

// upgradeRequiredHandler answers a plain request that reached a WebSocket-only
// path. 426 is the status defined for exactly this: the resource is there, but
// only over another protocol.
func upgradeRequiredHandler() http.Handler {
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.Header().Set("Upgrade", "websocket")
		responseWriter.Header().Set("Connection", "Upgrade")
		http.Error(
			responseWriter,
			"this path serves Connect over WebSocket; send an upgrade request",
			http.StatusUpgradeRequired,
		)
	})
}

// rejectUpgrade answers an upgrade that arrived at a bare procedure path while
// a prefix is configured. Left to fall through, it reaches the plain Connect
// handler, which answers the handshake's GET with 505 Version Not Supported and
// points whoever reads it at the wrong problem entirely.
func rejectUpgrade(next http.Handler) http.Handler {
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if wstransport.IsUpgrade(request) {
			http.Error(
				responseWriter,
				"this path serves Connect over HTTP; WebSocket is served under the configured path prefix",
				http.StatusBadRequest,
			)
			return
		}
		next.ServeHTTP(responseWriter, request)
	})
}

// websocketMux wraps every handler registered on mux with the WebSocket
// upgrade, so each route serves both transports.
type websocketMux struct {
	mux     ServeMux
	server  *connect.Server
	options []wstransport.ServerOption
	prefix  string
}

func (m *websocketMux) Handle(pattern string, handler http.Handler) {
	if m.prefix == "" {
		// Both transports share the path: an upgrade is served here, anything
		// else falls through to the HTTP handler underneath.
		m.mux.Handle(pattern, wstransport.Upgrade(m.server, handler, m.options...))
		return
	}
	// Split, so a load balancer can tell the two apart by URL. The bare path
	// stops accepting upgrades; the prefixed one accepts nothing else.
	m.mux.Handle(pattern, rejectUpgrade(handler))
	m.mux.Handle(m.prefix+pattern, http.StripPrefix(
		m.prefix,
		wstransport.Upgrade(m.server, upgradeRequiredHandler(), m.options...),
	))
}

// brokenTransport reports a configuration error from every RPC, which is where
// a caller will actually see it.
type brokenTransport struct{ err error }

func (t *brokenTransport) NewClientStream(context.Context, connect.Spec) (connect.ClientStream, error) {
	return nil, t.err
}
