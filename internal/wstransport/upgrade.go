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
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"github.com/coder/websocket"
)

// hijackerHelp is returned when the listener cannot hijack the connection,
// which is the usual symptom of an h2c-only server.
const hijackerHelp = "Connect over WebSocket requires the HTTP listener to support http.Hijacker; " +
	"see https://connectrpc.com/docs/go/deployment/#h2c for enabling HTTP/1.1"

// SessionInfo describes a connection that has completed the WebSocket
// handshake. Every field is resolved before the 101 response, because HTTP
// headers cannot be added once the connection is upgraded.
type SessionInfo struct {
	// Codec is the codec negotiated through the WebSocket subprotocol. Bodies
	// in both directions use it, in the frame type it calls for (§6.1).
	Codec connect.Codec
	// Subprotocol is the token echoed to the client in Sec-WebSocket-Protocol.
	Subprotocol string
	// PeerAddr is the client's address, for [connect.CallInfo].
	PeerAddr string
	// Request is the HTTP request that carried the upgrade. Its body has been
	// consumed by the handshake; it is provided for headers, URL, and context.
	Request *http.Request
	// OnProtocolError observes a client's framing mistakes; see
	// [WithServerProtocolErrorHandler]. It is nil unless one was registered.
	OnProtocolError ServerProtocolErrorHandler
	// MaxTimeout bounds the RPC; see [WithMaxTimeout]. Zero means no bound.
	MaxTimeout time.Duration
	// ReadMaxBytes and SendMaxBytes are the size limits configured on the
	// handler. They travel with the connection rather than being held by the
	// Session so that a Session supplied through WithSession is configured by
	// the same options as the handshake, with no second place for them to live
	// and disagree. Zero means no limit.
	ReadMaxBytes int
	SendMaxBytes int
	// InfrastructureHeaders names the request headers this deployment's own
	// proxies set, which a client may therefore not send in a Leading-Metadata
	// message. A trailing "*" matches any suffix. See WithInfrastructureHeaders.
	InfrastructureHeaders []string
	// KeepAliveInterval is how often the session pings the client; see
	// [WithKeepAlive]. Zero sends no Pings.
	KeepAliveInterval time.Duration
	// KeepAliveTimeout is how long the client may stay silent; see
	// [WithKeepAliveTimeout].
	KeepAliveTimeout time.Duration
	// HandshakeTimeout bounds the wait for the client's opening M message;
	// see [WithHandshakeTimeout]. Zero means no bound.
	HandshakeTimeout time.Duration
	// Procedure is the procedure this connection calls; see [WithProcedure].
	// Empty means the request path.
	Procedure string
	// TransportInfo is what [connect.CallInfo.TransportInfo] carries; see
	// [WithTransportInfo].
	TransportInfo any
	// drain caps what the closing handshake reads; see drainLimiter.
	drain *drainLimiter
	// heartbeat carries the peer's PINGs and PONGs to the session's reader.
	heartbeat *heartbeat
}

// Session serves RPCs on an upgraded WebSocket connection. Serve reads
// messages from conn, builds a [connect.ServerStream] per logical stream, and
// dispatches each through [connect.Server.Call]. It owns conn and closes it.
//
// Serve returns when the connection ends. Its error is logged, not sent: the
// HTTP response is already committed by the time it runs.
type Session interface {
	Serve(ctx context.Context, server *connect.Server, conn *websocket.Conn, info SessionInfo) error
}

// SessionFunc adapts a function to the [Session] interface.
type SessionFunc func(ctx context.Context, server *connect.Server, conn *websocket.Conn, info SessionInfo) error

// Serve implements [Session].
func (f SessionFunc) Serve(ctx context.Context, server *connect.Server, conn *websocket.Conn, info SessionInfo) error {
	return f(ctx, server, conn, info)
}

// Upgrade returns an [http.Handler] that serves WebSocket RPCs for server and
// delegates every other request to next. It lets one route carry both
// transports, so WebSocket streams and plain HTTP unary calls share a URL.
//
// The codec is negotiated before the handshake and reported on [SessionInfo].
// Message compression is the WebSocket transport's own permessage-deflate, not
// a Connect content encoding. A nil next responds 404 to non-WebSocket requests.
//
// RPCs are served by the [Session] from [WithSession], or by a default one
// built from these same options.
func Upgrade(server *connect.Server, next http.Handler, options ...ServerOption) http.Handler {
	opts := defaultOptions()
	for _, opt := range options {
		opt.applyToServer(&opts)
	}
	serve := opts.session
	if serve == nil {
		serve = &session{}
	}
	// §4.3: a server MUST support JSON, so a client that offers it can always
	// connect, whatever else the codecs were narrowed to.
	if _, ok := newCodecRegistry(opts.codecs).get(connect.CodecNameJSON); !ok {
		opts.codecs = append(slices.Clone(opts.codecs), connectproto.NewJSONCodec())
	}
	return &upgradeHandler{
		server:    server,
		session:   serve,
		next:      next,
		codecs:    newCodecRegistry(opts.codecs),
		carried:   carriedCodecs(opts.codecs),
		opts:      &opts,
		shutdowns: newShutdownTracker(),
	}
}

type upgradeHandler struct {
	server  *connect.Server
	session Session
	next    http.Handler
	codecs  registry[connect.Codec]
	// carried is the registered codecs a token can name, in registration
	// order, for the 400 body.
	carried []connect.Codec
	opts    *options
	// shutdowns ends this handler's sessions when their server shuts down.
	shutdowns *shutdownTracker
}

func (h *upgradeHandler) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	// Ahead of the IsUpgrade gate, which only recognizes GET: an Extended
	// CONNECT would otherwise fall through to the HTTP handler and be answered
	// as an ordinary RPC. RFC 8441 carries WebSocket over HTTP/2 and this
	// binding does not adopt it (§4.1), so the request is refused instead.
	if request.Method == http.MethodConnect {
		http.Error(
			responseWriter,
			"this server does not support Extended CONNECT (RFC 8441); "+
				"upgrade over HTTP/1.1 instead",
			http.StatusNotImplemented,
		)
		return
	}
	if !IsUpgrade(request) || h.session == nil {
		if h.next == nil {
			http.NotFound(responseWriter, request)
			return
		}
		h.next.ServeHTTP(responseWriter, request)
		return
	}
	subprotocol, codecName, admitted := h.admit(responseWriter, request)
	if !admitted {
		return
	}
	codec, _ := h.codecs.get(codecName)
	drain := newDrainLimiter(messageReadLimit(h.opts.readMaxBytes))
	beat := &heartbeat{}
	conn, upgradeErr := websocket.Accept(
		&drainResponseWriter{ResponseWriter: responseWriter, limiter: drain},
		request,
		h.acceptOptions(subprotocol, beat),
	)
	if upgradeErr != nil {
		// Accept has already written the error response.
		return
	}
	info := h.sessionInfo(request, codec, subprotocol, drain, beat)
	ctx, untrack := h.shutdowns.track(request.Context())
	defer untrack()
	if err := h.session.Serve(ctx, h.server, conn, info); err != nil {
		h.opts.logger.Error(
			"websocket session ended with error",
			"procedure", info.Procedure,
			"peer", request.RemoteAddr,
			"error", err,
		)
	}
}

// admit decides whether to complete an upgrade, and picks the subprotocol and
// codec when it does. A refusal has already been written to responseWriter.
func (h *upgradeHandler) admit(
	responseWriter http.ResponseWriter,
	request *http.Request,
) (subprotocol, codecName string, admitted bool) {
	// Checked first: a listener that cannot hijack is a deployment fault, and
	// reporting it as a client error would send the operator down the wrong path.
	if !canHijack(responseWriter) {
		http.Error(responseWriter, hijackerHelp, http.StatusInternalServerError)
		return "", "", false
	}
	// §4.4 before §4.3, so a cross-origin handshake is refused as one whatever
	// it offered. Accept is told to skip its own check, which would run too
	// late; WithCheckOrigin replaces this one rather than adding to it.
	checkOrigin := h.opts.checkOrigin
	if checkOrigin == nil {
		checkOrigin = isSameOrigin
	}
	if !checkOrigin(request) {
		http.Error(responseWriter, "origin not allowed", http.StatusForbidden)
		return "", "", false
	}
	// §4.3: an upgrade that offers no token this server can serve is refused
	// before the handshake completes, whether or not it offered a Connect token.
	subprotocol, codecName, negotiated := negotiateSubprotocol(
		requestedSubprotocols(request),
		h.servesCodec,
	)
	if !negotiated {
		http.Error(
			responseWriter,
			"no supported Connect subprotocol in Sec-WebSocket-Protocol; this server speaks "+
				strings.Join(h.servedSubprotocols(), ", "),
			http.StatusBadRequest,
		)
		return "", "", false
	}
	return subprotocol, codecName, true
}

func (h *upgradeHandler) acceptOptions(subprotocol string, beat *heartbeat) *websocket.AcceptOptions {
	return &websocket.AcceptOptions{
		// Accept echoes Sec-WebSocket-Protocol itself from the list it is given.
		Subprotocols: []string{subprotocol},
		// The origin policy has already run.
		InsecureSkipVerify: true,
		// §4.6: a peer's own PINGs and PONGs are frames from it too.
		OnPingReceived: beat.onPing,
		OnPongReceived: beat.onPong,
		// Compression is the transport's job, not the framing layer's: browsers
		// get permessage-deflate for free and cannot negotiate a Connect-level
		// encoding on an upgrade request.
		//
		// NoContextTakeover is required rather than preferred. A compression
		// context shared across messages leaks plaintext across trust
		// boundaries (the CRIME/BREACH family), and the server may impose the
		// parameter unilaterally under RFC 7692 §7.1.1, so a client offering
		// plain permessage-deflate still gets a per-message context.
		CompressionMode:      h.opts.compressionMode(),
		CompressionThreshold: h.opts.compressionThreshold(),
	}
}

func (h *upgradeHandler) sessionInfo(
	request *http.Request,
	codec connect.Codec,
	subprotocol string,
	drain *drainLimiter,
	beat *heartbeat,
) SessionInfo {
	procedure := h.opts.procedure
	if procedure == "" {
		procedure = request.URL.Path
	}
	var transportInfo any
	if h.opts.transportInfo != nil {
		transportInfo = h.opts.transportInfo(request)
	}
	return SessionInfo{
		Procedure:             procedure,
		TransportInfo:         transportInfo,
		HandshakeTimeout:      h.opts.handshakeTimeout,
		Codec:                 codec,
		Subprotocol:           subprotocol,
		PeerAddr:              request.RemoteAddr,
		Request:               request,
		MaxTimeout:            h.opts.maxTimeout,
		ReadMaxBytes:          h.opts.readMaxBytes,
		SendMaxBytes:          h.opts.sendMaxBytes,
		InfrastructureHeaders: h.opts.infrastructureHeaders,
		OnProtocolError:       h.opts.onProtocolError,
		drain:                 drain,
		heartbeat:             beat,
		KeepAliveInterval:     h.opts.keepAliveInterval,
		KeepAliveTimeout:      h.opts.keepAliveTimeout,
	}
}

// isSameOrigin is the default origin policy (§4.4): a request without Origin is
// not from a browser, and one with it must name the request's own host and
// port, compared case-insensitively and exactly as written.
func isSameOrigin(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return parsed.Host != "" && strings.EqualFold(parsed.Host, request.Host)
}

// canHijack reports whether responseWriter, or one it wraps, can hijack the
// connection, which is how [http.ResponseController] finds a Hijacker too.
func canHijack(responseWriter http.ResponseWriter) bool {
	for {
		if _, ok := responseWriter.(http.Hijacker); ok {
			return true
		}
		unwrapper, ok := responseWriter.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		responseWriter = unwrapper.Unwrap()
	}
}

// IsUpgrade reports whether request is a WebSocket upgrade, so callers can
// route it away from the HTTP handlers before reading a body.
func IsUpgrade(request *http.Request) bool {
	if request.Method != http.MethodGet {
		return false
	}
	if !headerContainsToken(request.Header, wsHeaderConnection, "upgrade") {
		return false
	}
	return strings.EqualFold(request.Header.Get(wsHeaderUpgrade), "websocket")
}

// servesCodec reports whether this server can encode and decode bodies with
// the named codec. JSON is always among them (§4.3).
func (h *upgradeHandler) servesCodec(name string) bool {
	_, ok := h.codecs.get(name)
	return ok
}

// servedSubprotocols lists the tokens a client could offer, for the 400 body.
func (h *upgradeHandler) servedSubprotocols() []string {
	tokens := make([]string, 0, len(h.carried))
	for _, codec := range h.carried {
		tokens = append(tokens, subprotocolForCodec(codec.Name()))
	}
	return tokens
}

// requestedSubprotocols parses the client's Sec-WebSocket-Protocol offer, in
// the order it was offered.
func requestedSubprotocols(request *http.Request) []string {
	var protocols []string
	for _, value := range request.Header.Values(wsHeaderProtocol) {
		for part := range strings.SplitSeq(value, ",") {
			if token := strings.TrimSpace(part); token != "" {
				protocols = append(protocols, token)
			}
		}
	}
	return protocols
}

// negotiateSubprotocol picks the first offered subprotocol this server both
// recognizes and can serve, as RFC 6455 §4.1 negotiation does. Tokens it does
// not recognize, and recognized tokens whose codec it lacks, are skipped, so a
// client offering JSON first still reaches a Protobuf-only server through its
// second choice.
func negotiateSubprotocol(
	requested []string,
	supported func(string) bool,
) (subprotocol, codecName string, ok bool) {
	for _, name := range requested {
		codec := codecForSubprotocol(name)
		if codec != "" && supported(codec) {
			return name, codec, true
		}
	}
	return "", "", false
}

func headerContainsToken(header http.Header, key, token string) bool {
	for _, value := range header.Values(key) {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// registry is a name-keyed lookup for codecs.
type registry[T interface{ Name() string }] struct {
	byName map[string]T
}

func newCodecRegistry(codecs []connect.Codec) registry[connect.Codec] {
	return newRegistry(codecs)
}

func newRegistry[T interface{ Name() string }](entries []T) registry[T] {
	reg := registry[T]{byName: make(map[string]T, len(entries))}
	for _, entry := range entries {
		name := entry.Name()
		if _, dup := reg.byName[name]; dup {
			continue
		}
		reg.byName[name] = entry
	}
	return reg
}

func (r registry[T]) get(name string) (T, bool) {
	entry, ok := r.byName[name]
	return entry, ok
}
