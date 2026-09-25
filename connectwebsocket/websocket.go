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

// Package connectwebsocket carries Connect RPCs over WebSocket. It multiplexes
// every stream of a call onto one connection instead of the request per RPC
// that HTTP forces, which is what makes bidirectional streaming work on
// HTTP/1.1 and in browsers.
//
// The package composes with [connectrpc.com/connect/v2/connecthttp] rather
// than replacing it. On the client, [NewTransport] sends streaming RPCs over
// WebSocket and builds a connecthttp transport for everything else, so one set
// of options configures both. On the server, [Mount] registers each procedure
// over both transports, letting a WebSocket upgrade and a plain HTTP request
// share one URL.
package connectwebsocket

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectgzip"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"github.com/coder/websocket"
)

const defaultHandshakeTimeout = 10 * time.Second

// defaultMaxTimeout bounds an RPC that requested no deadline of its own. It is
// generous because a long-lived subscription is the transport's whole point; it
// exists so that a peer cannot hold a connection open forever — including one
// that upgrades and never sends the metadata that opens the stream. Override it
// with [WithMaxTimeout].
const defaultMaxTimeout = time.Hour

// defaultReadMaxBytes matches connecthttp's default so that one set of options
// gives both transports the same limit. A stream-type-dependent limit is a
// bug waiting to happen.
const defaultReadMaxBytes = 1024 * 1024 * 4 // 4MiB

// defaultCompressMinBytes is set for the same reason, and to the value the
// WebSocket library would otherwise pick for itself. Left unset, the two halves
// disagree: the library treats zero as "use my default" and skips messages
// under 512 bytes, while the message writer treats zero as "no minimum" and
// compresses everything. Small payloads usually grow under deflate, so 512 is
// the better answer for both.
const defaultCompressMinBytes = 512

// ClientOption configures [NewTransport].
type ClientOption interface {
	applyToClient(*options)
}

// ServerOption configures [Upgrade], [Mux], and [Mount].
type ServerOption interface {
	applyToServer(*options)
}

// Option configures either side. Size limits, codecs, and compression settings
// are Options because both ends of a connection need them; anything meaningful
// to only one side is a [ClientOption] or a [ServerOption], so passing it to
// the wrong constructor is a compile error rather than a setting that is
// silently ignored.
type Option interface {
	ClientOption
	ServerOption
}

// WithSelector replaces the default [SelectStreaming] routing rule, which
// decides which RPCs a transport sends over WebSocket.
//
// It is a client-side choice only. A server accepts whichever transport a
// client picked, so [Mount] serves every procedure both ways rather than
// consulting a selector of its own.
func WithSelector(selector Selector) ClientOption {
	return clientOptionFunc(func(o *options) {
		if selector != nil {
			o.selector = selector
		}
	})
}

// WithFallbackOnUpgradeError retries an RPC on the fallback transport when the
// WebSocket handshake fails. It hides a peer that does not speak this
// transport, so it also hides a misconfigured one; it is off by default.
func WithFallbackOnUpgradeError() ClientOption {
	return clientOptionFunc(func(o *options) { o.fallbackOnUpgradeError = true })
}

// WithHandshakeTimeout bounds the WebSocket handshake. Zero means no timeout.
func WithHandshakeTimeout(timeout time.Duration) ClientOption {
	return clientOptionFunc(func(o *options) { o.handshakeTimeout = timeout })
}

// WithCheckOrigin replaces the default origin check.
//
// By default a cross-origin handshake is refused: a browser attaches the
// user's cookies to a WebSocket handshake and, unlike fetch, no CORS
// preflight stands in the way, so any page on any site could otherwise open
// an authenticated stream (cross-site WebSocket hijacking). A request with no
// Origin header is allowed, which covers every non-browser client.
//
// Pass a function returning true to accept every origin — appropriate only
// when the endpoint carries no ambient credentials.
func WithCheckOrigin(check func(*http.Request) bool) ServerOption {
	return serverOptionFunc(func(o *options) {
		if check != nil {
			o.checkOrigin = check
		}
	})
}

// WithSession replaces the [Session] that serves RPCs on an upgraded
// connection. The default is built from the same options as the handshake, so
// it needs no configuring; supply one only to change how streams are served —
// to instrument them, or to dispatch differently. Wrapping [NewSession] keeps
// the default behavior underneath.
//
// A nil Session is ignored.
func WithSession(session Session) ServerOption {
	return serverOptionFunc(func(o *options) {
		if session != nil {
			o.session = session
		}
	})
}

// WithHTTPOptions passes options through to the [connecthttp] handlers [Mount]
// registers. The options both packages share — codecs, compressors, size
// limits, the compression threshold — are forwarded already; use this to reach
// the ones only connecthttp has.
func WithHTTPOptions(httpOptions ...connecthttp.Option) ServerOption {
	return serverOptionFunc(func(o *options) { o.httpOptions = httpOptions })
}

// ProtocolFault classifies a peer's framing mistake, so a monitor can tell a
// client that consistently sends unknown markers from one that mismatches a
// frame type once. The Connect code alone cannot: most framing faults are
// InvalidArgument, separable only by matching message strings.
type ProtocolFault int

const (
	// FaultUnknown is a peer fault this package has not classified.
	FaultUnknown ProtocolFault = iota
	// FaultMarker is a marker that is unknown, belongs to the other direction,
	// or sets the reserved high bit.
	FaultMarker
	// FaultFrameType is a frame whose type does not match its payload: an
	// empty body in a text frame, or a message type that is neither.
	FaultFrameType
	// FaultSizeLimit is a message past the configured read limit.
	FaultSizeLimit
	// FaultMetadata is a malformed, missing, or repeated M message.
	FaultMetadata
	// FaultMessageEncoding is a payload the negotiated codec cannot decode.
	FaultMessageEncoding
)

func (f ProtocolFault) String() string {
	switch f {
	case FaultMarker:
		return "marker"
	case FaultFrameType:
		return "frame_type"
	case FaultSizeLimit:
		return "size_limit"
	case FaultMetadata:
		return "metadata"
	case FaultMessageEncoding:
		return "message_encoding"
	case FaultUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// ServerProtocolErrorHandler observes a client's framing mistake. It cannot
// change the outcome: the error is reported to the peer and returned to the
// handler either way, so this is for logging, metrics, and rate limiting only.
//
// It runs on the connection's read path, so it must not block or panic.
type ServerProtocolErrorHandler func(SessionInfo, ProtocolFault, *connect.Error)

// ClientProtocolErrorHandler observes a server's framing mistake. Like its
// server-side twin it cannot change the outcome; the [connect.Spec] says which
// RPC was in flight, the peer being whichever server the transport dials.
//
// It runs on the connection's read path, so it must not block or panic.
type ClientProtocolErrorHandler func(connect.Spec, ProtocolFault, *connect.Error)

// WithServerProtocolErrorHandler registers a [ServerProtocolErrorHandler]. Use
// it to attribute framing faults to a client — [SessionInfo] carries PeerAddr
// and the upgrade request — which the logger cannot do: a framing fault is
// reported to the peer in the S message and is a successful outcome from the
// session's point of view, so it never reaches [WithLogger].
func WithServerProtocolErrorHandler(handler ServerProtocolErrorHandler) ServerOption {
	return serverOptionFunc(func(o *options) {
		if handler != nil {
			o.onProtocolError = handler
		}
	})
}

// WithClientProtocolErrorHandler registers a [ClientProtocolErrorHandler], the
// mirror of [WithServerProtocolErrorHandler]: it reports a server that frames
// its responses wrongly. A client has no equivalent of the logger or the
// session wrapper, so without this a framing fault is visible only as the
// InvalidArgument the RPC fails with.
func WithClientProtocolErrorHandler(handler ClientProtocolErrorHandler) ClientOption {
	return clientOptionFunc(func(o *options) {
		if handler != nil {
			o.onClientProtocolError = handler
		}
	})
}

// WithPathPrefix serves WebSocket RPCs under a path prefix, so that every
// upgrade on the wire is distinguishable by URL alone. Some load balancers
// need that to route WebSocket traffic differently from ordinary requests —
// sticky backends, longer idle timeouts, upgrade support enabled.
//
// Both ends need the same value: the client dials
// baseURL + prefix + procedure, and the server registers its WebSocket
// handlers there. Plain HTTP RPCs keep the bare procedure paths, and when a
// prefix is set the bare paths no longer accept upgrades — a split the load
// balancer can rely on is the entire point.
//
// A request under the prefix that is not an upgrade is answered
// 426 Upgrade Required.
//
// The prefix is normalized to a single leading slash and no trailing slash, so
// "ws", "/ws" and "/ws/" are the same. The empty string disables it, which is
// the default.
func WithPathPrefix(prefix string) Option {
	return optionFunc(func(o *options) { o.pathPrefix = normalizePathPrefix(prefix) })
}

// normalizePathPrefix makes a prefix safe to concatenate with a procedure,
// which always begins with a slash.
func normalizePathPrefix(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return ""
	}
	return "/" + prefix
}

// WithMaxTimeout bounds every RPC the handler serves. It is both the default
// when a client requests no deadline and the ceiling on one that does: the
// effective deadline is the shorter of the two, so a client may ask for less
// time but never more.
//
// Zero means no bound, which lets a peer that opens a connection and then goes
// silent hold it indefinitely.
func WithMaxTimeout(timeout time.Duration) ServerOption {
	return serverOptionFunc(func(o *options) { o.maxTimeout = timeout })
}

// WithLogger replaces [slog.Default] as the destination for errors that
// surface after the HTTP response is committed.
func WithLogger(logger *slog.Logger) ServerOption {
	return serverOptionFunc(func(o *options) {
		if logger != nil {
			o.logger = logger
		}
	})
}

// WithCodecs replaces the default protobuf binary and JSON codecs. The set is
// replaced entirely, so list every codec you need in one call. A codec is
// reachable only if a WebSocket subprotocol names it.
func WithCodecs(codecs ...connect.Codec) Option {
	return optionFunc(func(o *options) { o.codecs = codecs })
}

// WithCompressors replaces the default gzip compressor on the HTTP half: the
// client's fallback transport, and the handlers [Mount] registers. It has no
// effect on WebSocket traffic, which uses the transport's own
// permessage-deflate rather than a Connect content encoding.
func WithCompressors(compressors ...connect.Compressor) Option {
	return optionFunc(func(o *options) { o.compressors = compressors })
}

// WithSendCodec selects the codec for outgoing messages by name. It must name
// one of the codecs in the set, and it also selects the WebSocket subprotocol
// the client offers.
func WithSendCodec(name string) ClientOption {
	return clientOptionFunc(func(o *options) { o.sendCodec = name })
}

// WithSendCompression selects the compressor for outgoing messages on the HTTP
// fallback. The empty string and [connect.CompressionNameIdentity] both mean
// uncompressed. It has no effect on WebSocket traffic.
func WithSendCompression(name string) ClientOption {
	return clientOptionFunc(func(o *options) { o.sendCompression = name })
}

// WithoutCompression disables permessage-deflate on this side of the
// connection. Either side can disable it unilaterally, so one call is enough.
//
// The usual reason is security rather than performance. Per-message contexts
// stop a compressed message from leaking the content of earlier ones, but they
// do not stop a single message from leaking: caller-influenced data sitting
// beside a secret still shows up in the compressed length. A service whose
// responses mix the two should turn compression off rather than rely on the
// narrower guarantee.
//
// It is also worth turning off for payloads that do not compress — already
// encoded media, or protobuf binary — where deflate costs CPU for no gain.
func WithoutCompression() Option {
	return optionFunc(func(o *options) { o.compressionDisabled = true })
}

// WithCompressMinBytes sets the size below which messages are sent
// uncompressed, since compressing a small payload usually makes it bigger. It
// applies to both halves: permessage-deflate's threshold on WebSocket, and the
// Connect content encoding on the HTTP fallback.
//
// The default is 512 bytes. Zero means no minimum — compress everything.
func WithCompressMinBytes(minBytes int) Option {
	return optionFunc(func(o *options) { o.compressMinBytes = minBytes })
}

// WithInfrastructureHeaders replaces the request headers a client may not set
// in its Leading-Metadata message because this deployment's own infrastructure
// sets them. A trailing "*" matches any suffix, so "X-Forwarded-*" covers the
// family. Matching is case-insensitive.
//
// The default is Forwarded, X-Forwarded-*, and X-Real-IP: what a proxy in
// front of the server sets, and what a client must not be able to forge. Which
// names a deployment's infrastructure actually controls is a property of that
// deployment, so this replaces the list rather than adding to it — pass no
// names to allow all of them.
//
// It does not affect the names the Fetch standard forbids as request headers,
// or the ones this protocol controls. Those are reserved whatever a deployment
// configures.
func WithInfrastructureHeaders(names ...string) ServerOption {
	return serverOptionFunc(func(o *options) {
		o.infrastructureHeaders = names
	})
}

// WithReadMaxBytes bounds the size of a message this side will accept. Zero
// means no limit.
func WithReadMaxBytes(maxBytes int) Option {
	return optionFunc(func(o *options) { o.readMaxBytes = maxBytes })
}

// WithSendMaxBytes bounds the size of a message this side will send. Zero
// means no limit.
func WithSendMaxBytes(maxBytes int) Option {
	return optionFunc(func(o *options) { o.sendMaxBytes = maxBytes })
}

// WithHTTPClient replaces [http.DefaultClient] for both halves of the
// transport: the WebSocket dial and the HTTP fallback share it, so TLS, proxy,
// and dialer settings cannot differ between them.
//
// A transport that is not an [*http.Client] can still be used for the HTTP
// half alone through [WithFallbackTransport].
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return clientOptionFunc(func(o *options) {
		if httpClient != nil {
			o.httpClient = httpClient
		}
	})
}

// WithFallbackTransport replaces the automatic [connecthttp] fallback. Use it
// to reach HTTP-only options such as connecthttp.WithHTTPGet, or to supply a
// test double.
func WithFallbackTransport(transport connect.Transport) ClientOption {
	return clientOptionFunc(func(o *options) { o.fallbackTransport = transport })
}

// ServeMux is the subset of [http.ServeMux] used by [Mount] and [Mux].
// Any router that satisfies this interface can host Connect routes.
type ServeMux interface {
	// Handle registers handler for pattern.
	Handle(pattern string, handler http.Handler)
}

// Mux returns a [ServeMux] that wraps every handler registered on mux with
// [Upgrade], so each route serves WebSocket RPCs and falls through to the
// original handler for everything else.
//
// [Mount] does this for you; reach for Mux when you need to drive
// [connectrpc.com/connect/v2/connecthttp.Mount] yourself — to pass it options
// Mount does not forward, or to register routes of your own alongside:
//
//	connecthttp.Mount(connectwebsocket.Mux(mux, server), server, httpOptions...)
func Mux(mux ServeMux, server *connect.Server, options ...ServerOption) ServeMux {
	opts := defaultOptions()
	for _, opt := range options {
		opt.applyToServer(&opts)
	}
	return &upgradeMux{mux: mux, server: server, options: options, prefix: opts.pathPrefix}
}

type upgradeMux struct {
	mux     ServeMux
	server  *connect.Server
	options []ServerOption
	prefix  string
}

func (m *upgradeMux) Handle(pattern string, handler http.Handler) {
	if m.prefix == "" {
		// Both transports share the path: an upgrade is served here, anything
		// else falls through to the HTTP handler underneath.
		m.mux.Handle(pattern, Upgrade(m.server, handler, m.options...))
		return
	}
	// Split, so a load balancer can tell the two apart by URL. The bare path
	// stops accepting upgrades; the prefixed one accepts nothing else.
	m.mux.Handle(pattern, handler)
	m.mux.Handle(m.prefix+pattern, http.StripPrefix(
		m.prefix,
		Upgrade(m.server, upgradeRequiredHandler(), m.options...),
	))
}

// upgradeRequiredHandler answers a plain request that reached a WebSocket-only
// path. 426 is the status defined for exactly this — the resource is there, but
// only over another protocol.
func upgradeRequiredHandler() http.Handler {
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.Header().Set(wsHeaderUpgrade, "websocket")
		responseWriter.Header().Set(wsHeaderConnection, "Upgrade")
		http.Error(
			responseWriter,
			"this path serves Connect over WebSocket; send an upgrade request",
			http.StatusUpgradeRequired,
		)
	})
}

// Mount registers every procedure on mux, served over WebSocket to clients
// that upgrade and over HTTP to clients that do not. One call replaces
// [connectrpc.com/connect/v2/connecthttp.Mount] wrapped around [Mux].
//
// Both transports are registered on every path deliberately. Which transport
// carries an RPC is the client's choice — see [WithSelector] — so a server
// that served only one would reject a client that chose the other. Use [Mux]
// directly to compose with your own connecthttp.Mount call.
func Mount(mux ServeMux, server *connect.Server, options ...ServerOption) {
	opts := defaultOptions()
	for _, opt := range options {
		opt.applyToServer(&opts)
	}
	connecthttp.Mount(Mux(mux, server, options...), server, serverHTTPOptions(&opts)...)
}

// serverHTTPOptions forwards the settings both packages share to the HTTP
// handlers, so one option list configures whichever way an RPC arrives. It is
// the server-side twin of httpOptions.
func serverHTTPOptions(opts *options) []connecthttp.Option {
	const shared = 5
	forwarded := make([]connecthttp.Option, 0, shared+len(opts.httpOptions))
	forwarded = append(forwarded,
		connecthttp.WithCodecs(opts.codecs...),
		connecthttp.WithCompressors(opts.compressors...),
		connecthttp.WithCompressMinBytes(opts.compressMinBytes),
		connecthttp.WithReadMaxBytes(opts.readMaxBytes),
		connecthttp.WithSendMaxBytes(opts.sendMaxBytes),
	)
	return append(forwarded, opts.httpOptions...)
}

type options struct {
	session                Session
	onProtocolError        ServerProtocolErrorHandler
	onClientProtocolError  ClientProtocolErrorHandler
	httpOptions            []connecthttp.Option
	maxTimeout             time.Duration
	pathPrefix             string
	selector               Selector
	fallbackOnUpgradeError bool
	handshakeTimeout       time.Duration
	checkOrigin            func(*http.Request) bool
	logger                 *slog.Logger
	codecs                 []connect.Codec
	compressors            []connect.Compressor
	sendCodec              string
	sendCompression        string
	compressMinBytes       int
	compressionDisabled    bool
	readMaxBytes           int
	sendMaxBytes           int
	infrastructureHeaders  []string
	httpClient             *http.Client
	fallbackTransport      connect.Transport
}

func defaultOptions() options {
	return options{
		selector:         SelectStreaming,
		handshakeTimeout: defaultHandshakeTimeout,
		maxTimeout:       defaultMaxTimeout,
		// nil defers to the WebSocket library's own same-origin check; see
		// WithCheckOrigin.
		checkOrigin:           nil,
		logger:                slog.Default(),
		infrastructureHeaders: defaultInfrastructureHeaders(),
		codecs: []connect.Codec{
			connectproto.NewBinaryCodec(),
			connectproto.NewJSONCodec(),
		},
		compressors:      []connect.Compressor{connectgzip.New()},
		sendCodec:        connect.CodecNameProto,
		readMaxBytes:     defaultReadMaxBytes,
		compressMinBytes: defaultCompressMinBytes,
		httpClient:       http.DefaultClient,
	}
}

// compressionThreshold is the deflate threshold to hand the WebSocket library.
// Zero is that library's sentinel for "use my default", so an explicit request
// for no minimum has to be expressed as one byte to mean the same thing on both
// halves of the transport.
func (o *options) compressionThreshold() int {
	if o.compressMinBytes <= 0 {
		return 1
	}
	return o.compressMinBytes
}

// compressionMode is the permessage-deflate setting these options ask for.
// NoContextTakeover is the only enabled mode: a context shared across messages
// is the CRIME/BREACH exposure, so it is not offered.
func (o *options) compressionMode() websocket.CompressionMode {
	if o.compressionDisabled {
		return websocket.CompressionDisabled
	}
	return websocket.CompressionNoContextTakeover
}

// optionFunc satisfies both sides, so a shared option is one function value.
type optionFunc func(*options)

func (f optionFunc) applyToClient(o *options) { f(o) }
func (f optionFunc) applyToServer(o *options) { f(o) }

type clientOptionFunc func(*options)

func (f clientOptionFunc) applyToClient(o *options) { f(o) }

type serverOptionFunc func(*options)

func (f serverOptionFunc) applyToServer(o *options) { f(o) }
