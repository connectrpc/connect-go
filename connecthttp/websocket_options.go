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

// The WebSocket transport's public surface. The implementation is in
// internal/wstransport: Connect is protobuf-first, so which wire carries an
// RPC is an implementation detail, and a caller configures it through the
// options here rather than assembling a transport by hand.

package connecthttp

import (
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/wstransport"
)

// Selector reports whether spec's RPC should travel over WebSocket. A false
// result leaves it on HTTP. See [WithWebSocket].
type Selector func(spec connect.Spec) bool

// SelectStreaming routes every streaming RPC over WebSocket and leaves unary
// RPCs on HTTP, where a handshake would buy nothing. It is the usual choice.
func SelectStreaming(spec connect.Spec) bool {
	return spec.StreamType != connect.StreamTypeUnary
}

// SelectBidi routes only bidirectional RPCs over WebSocket. Server-streaming
// works over plain HTTP, so this is the narrower alternative to
// [SelectStreaming].
func SelectBidi(spec connect.Spec) bool {
	return spec.StreamType == connect.StreamTypeBidi
}

// SelectAll routes every RPC over WebSocket, including unary ones.
func SelectAll(connect.Spec) bool {
	return true
}

// ProtocolNameConnectWebSocket is what [connect.CallInfo].Protocol reports for
// an RPC carried over WebSocket, as connect.ProtocolNameConnect is for one
// over HTTP.
const ProtocolNameConnectWebSocket = wstransport.ProtocolConnectWebSocket

// IsWebSocketUpgrade reports whether request is a WebSocket handshake, so
// middleware can tell the two transports apart before either handler runs.
func IsWebSocketUpgrade(request *http.Request) bool {
	return wstransport.IsUpgrade(request)
}

// ProtocolFault classifies a peer's framing mistake, so a monitor can tell a
// client that consistently sends unknown markers from one that mismatches a
// frame type once. The Connect code alone cannot: most framing faults are
// InvalidArgument, separable only by matching message strings.
type ProtocolFault = wstransport.ProtocolFault

// The framing mistakes a peer can make. See [ProtocolFault].
const (
	// FaultUnknown is a peer fault this package has not classified.
	FaultUnknown = wstransport.FaultUnknown
	// FaultMarker is a marker that is unknown, belongs to the other direction,
	// or sets the reserved high bit.
	FaultMarker = wstransport.FaultMarker
	// FaultFrameType is a frame whose type does not match its payload.
	FaultFrameType = wstransport.FaultFrameType
	// FaultSizeLimit is a message past the configured read limit.
	FaultSizeLimit = wstransport.FaultSizeLimit
	// FaultMetadata is a malformed, missing, or repeated metadata message.
	FaultMetadata = wstransport.FaultMetadata
	// FaultMessageEncoding is a payload the negotiated codec cannot decode.
	FaultMessageEncoding = wstransport.FaultMessageEncoding
)

// WithWebSocketCheckOrigin replaces the default same-origin check on the
// handshake. Returning false refuses it with 403.
//
// The default matters: a browser attaches the user's cookies to a WebSocket
// handshake and performs no CORS preflight, so an allow-all policy lets any
// page open an authenticated stream. A handshake carrying no Origin header is
// not from a browser and is allowed either way.
func WithWebSocketCheckOrigin(check func(*http.Request) bool) Option {
	return webSocketServerOptionsOption{wstransport.WithCheckOrigin(check)}
}

// WithWebSocketMaxTimeout bounds every WebSocket RPC, whatever deadline the
// client asked for. The effective deadline is the shorter of the two.
//
// The bound is what makes waiting for a client's opening message safe: without
// it, a peer that upgrades and then goes silent holds the connection forever.
// The default is an hour — long-lived streams are the reason this transport
// exists, and a short default would sever the subscriptions it carries.
func WithWebSocketMaxTimeout(timeout time.Duration) Option {
	return webSocketServerOptionsOption{wstransport.WithMaxTimeout(timeout)}
}

// WithWebSocketHandshakeTimeout bounds the handshake. The connection outlives
// it; only the upgrade is bounded.
func WithWebSocketHandshakeTimeout(timeout time.Duration) Option {
	return webSocketClientOptionsOption{wstransport.WithHandshakeTimeout(timeout)}
}

// WithWebSocketInfrastructureHeaders replaces the request headers a client may
// not set in its metadata message because this deployment's own infrastructure
// sets them. A trailing "*" matches any suffix.
//
// The default is Forwarded, X-Forwarded-*, and X-Real-IP. It does not affect
// the names the Fetch standard forbids as request headers, or the ones the
// protocol controls; those are reserved whatever a deployment configures.
func WithWebSocketInfrastructureHeaders(names ...string) Option {
	return webSocketServerOptionsOption{wstransport.WithInfrastructureHeaders(names...)}
}

// WithWebSocketLogger replaces [slog.Default] for WebSocket session errors.
func WithWebSocketLogger(logger *slog.Logger) Option {
	return webSocketServerOptionsOption{wstransport.WithLogger(logger)}
}

// ServerProtocolErrorHandler observes a client's framing mistake. It cannot
// change the outcome: the error reaches the peer and the handler either way,
// so this is for logging, metrics, and rate limiting.
type ServerProtocolErrorHandler func(peerAddr string, request *http.Request, fault ProtocolFault, err *connect.Error)

// WithWebSocketServerProtocolErrorHandler registers a handler for framing
// faults a client commits.
//
// A framing fault is reported to the peer and is a successful outcome from the
// session's point of view, so it never reaches [WithWebSocketLogger]. This is
// the hook that says which kind of mistake it was and who made it.
func WithWebSocketServerProtocolErrorHandler(handler ServerProtocolErrorHandler) Option {
	if handler == nil {
		return webSocketServerOptionsOption(nil)
	}
	return webSocketServerOptionsOption{wstransport.WithServerProtocolErrorHandler(
		func(info wstransport.SessionInfo, fault wstransport.ProtocolFault, err *connect.Error) {
			handler(info.PeerAddr, info.Request, fault, err)
		},
	)}
}

// ClientProtocolErrorHandler observes a server's framing mistake, the mirror
// of [ServerProtocolErrorHandler].
type ClientProtocolErrorHandler func(spec connect.Spec, fault ProtocolFault, err *connect.Error)

// WithWebSocketClientProtocolErrorHandler registers a handler for framing
// faults a server commits, which is the only way a client learns that a server
// misframed rather than simply failed.
func WithWebSocketClientProtocolErrorHandler(handler ClientProtocolErrorHandler) Option {
	if handler == nil {
		return webSocketClientOptionsOption(nil)
	}
	return webSocketClientOptionsOption{wstransport.WithClientProtocolErrorHandler(
		func(spec connect.Spec, fault wstransport.ProtocolFault, err *connect.Error) {
			handler(spec, fault, err)
		},
	)}
}

// WithoutWebSocketCompression disables permessage-deflate on the handshake,
// leaving WebSocket messages uncompressed. It has no effect on the HTTP half,
// which negotiates a Connect content encoding instead.
//
// Compression is on by default, with no-context-takeover imposed in both
// directions. Turn it off when the payloads are already compressed, or when
// the CPU matters more than the bytes.
func WithoutWebSocketCompression() Option {
	return webSocketBothOption{wstransport.WithoutCompression()}
}

// WithWebSocketFaultCloseCode sends a peer that breaks the framing the close
// code the protocol assigns its fault, instead of hanging up on it.
//
// The protocol asks for that code, and it is the only part of a verdict a
// browser script can read: the WebSocket API hands a script the close code and
// never the message body. It is off by default all the same, because sending it
// means completing the closing handshake, and that handshake reads until the
// peer's own close frame arrives — bounded at five seconds, but by no number of
// bytes. A peer that just overran the read limit has bytes queued to spend them
// on: a rejected compression bomb was measured draining 128MiB inside the
// window, against 96KiB for hanging up. Nothing is decompressed or kept, so the
// cost is socket time rather than memory, and the connection is held for the
// duration either way.
//
// The end-of-stream message carrying the verdict is sent regardless; only the
// close code is given up. Turn this on where peers are browsers, or otherwise
// trusted enough that their diagnostics are worth the window.
func WithWebSocketFaultCloseCode() Option {
	return webSocketBothOption{wstransport.WithFaultCloseCode()}
}

// WithWebSocketKeepAlive sets how often each side sends a WebSocket Ping, so
// that a proxy which drops idle connections sees traffic on a quiet stream.
// The default is 30 seconds; zero or less turns keep-alive off.
//
// A peer heard nothing from for two intervals, neither a message nor a Pong,
// is hung up on with close code 1011 from a server or 3111 from a client, and
// the RPC fails. That includes a peer whose application has stopped taking
// messages, since it then stops reading its connection too.
func WithWebSocketKeepAlive(interval time.Duration) Option {
	return webSocketBothOption{wstransport.WithKeepAlive(interval)}
}

// webSocketBothOption carries a setting that applies to whichever side is
// being configured.
type webSocketBothOption [1]wstransport.Option

func (o webSocketBothOption) apply(opts *options) {
	opts.websocketServerOptions = append(opts.websocketServerOptions, o[0])
	opts.websocketClientOptions = append(opts.websocketClientOptions, o[0])
}
