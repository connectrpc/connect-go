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
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
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
	// Codec is the codec negotiated through the WebSocket subprotocol. It is
	// the default for bodies this server sends; an incoming body is decoded
	// with whichever codec its frame type names.
	Codec connect.Codec
	// Codecs are both codecs the frame type selects between, since a client
	// may send either on any connection.
	Codecs codecPair
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
	// The error is not fatal here, unlike on the client: a handler with
	// neither codec negotiates nothing, so every upgrade is refused with a 415
	// naming the empty set rather than failing at some later point.
	pair, _ := newCodecPair(opts.codecs)
	return &upgradeHandler{
		server:    server,
		session:   serve,
		next:      next,
		codecs:    newCodecRegistry(opts.codecs),
		codecPair: pair,
		opts:      &opts,
	}
}

type upgradeHandler struct {
	server    *connect.Server
	session   Session
	next      http.Handler
	codecs    registry[connect.Codec]
	codecPair codecPair
	opts      *options
}

func (h *upgradeHandler) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	if !IsUpgrade(request) || h.session == nil {
		if h.next == nil {
			http.NotFound(responseWriter, request)
			return
		}
		h.next.ServeHTTP(responseWriter, request)
		return
	}
	// Checked first: a listener that cannot hijack is a deployment fault, and
	// reporting it as a client error would send the operator down the wrong path.
	if _, hijackable := responseWriter.(http.Hijacker); !hijackable {
		http.Error(responseWriter, hijackerHelp, http.StatusInternalServerError)
		return
	}
	// A custom policy is applied here and Accept is told to skip its own, so
	// that WithCheckOrigin fully replaces the check rather than adding to it.
	// Without one, Accept's same-origin check stands.
	if h.opts.checkOrigin != nil && !h.opts.checkOrigin(request) {
		http.Error(responseWriter, "origin not allowed", http.StatusForbidden)
		return
	}
	subprotocol, codecName, outcome := negotiateSubprotocol(
		requestedSubprotocols(request),
		h.servesCodec,
	)
	switch outcome {
	case negotiationUnrecognized:
		http.Error(
			responseWriter,
			"no supported WebSocket subprotocol in Sec-WebSocket-Protocol",
			http.StatusBadRequest,
		)
		return
	case negotiationUnsupportedCodec:
		http.Error(
			responseWriter,
			fmt.Sprintf(
				"no message encoding in common: this server speaks %v",
				h.servedCodecNames(),
			),
			http.StatusUnsupportedMediaType,
		)
		return
	case negotiationOK:
	}
	codec, _ := h.codecs.get(codecName)
	// Accept echoes Sec-WebSocket-Protocol itself from the list it is given.
	conn, upgradeErr := websocket.Accept(responseWriter, request, &websocket.AcceptOptions{
		Subprotocols: []string{subprotocol},
		// Only when WithCheckOrigin supplied a policy, which has already run.
		InsecureSkipVerify: h.opts.checkOrigin != nil,
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
	})
	if upgradeErr != nil {
		// Accept has already written the error response.
		return
	}
	info := SessionInfo{
		Codec:                 codec,
		Codecs:                h.codecPair,
		Subprotocol:           subprotocol,
		PeerAddr:              request.RemoteAddr,
		Request:               request,
		MaxTimeout:            h.opts.maxTimeout,
		ReadMaxBytes:          h.opts.readMaxBytes,
		SendMaxBytes:          h.opts.sendMaxBytes,
		InfrastructureHeaders: h.opts.infrastructureHeaders,
		OnProtocolError:       h.opts.onProtocolError,
	}
	if err := h.session.Serve(request.Context(), h.server, conn, info); err != nil {
		h.opts.logger.Error(
			"websocket session ended with error",
			"procedure", request.URL.Path,
			"peer", request.RemoteAddr,
			"error", err,
		)
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
// the named codec. A server MAY be configured with only one of the two.
func (h *upgradeHandler) servesCodec(name string) bool {
	_, ok := h.codecs.get(name)
	return ok
}

// servedCodecNames lists the codecs a client could negotiate, for the 415
// body. It is the registry filtered to the two this binding can carry, not
// every codec the server happens to hold.
func (h *upgradeHandler) servedCodecNames() []string {
	var names []string
	for _, name := range []string{connect.CodecNameProto, connect.CodecNameJSON} {
		if h.servesCodec(name) {
			names = append(names, name)
		}
	}
	return names
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

// negotiationOutcome distinguishes a client that is not speaking this protocol
// from one that is but offered an encoding this server does not have. The
// remedies differ, and so do the HTTP statuses.
type negotiationOutcome int

const (
	negotiationOK negotiationOutcome = iota
	negotiationUnrecognized
	negotiationUnsupportedCodec
)

// codecForSubprotocol maps a token to the codec it selects, or "" if the token
// is not one of this binding's.
func codecForSubprotocol(name string) string {
	switch name {
	case wsSubprotocolProto:
		return connect.CodecNameProto
	case wsSubprotocolJSON, wsSubprotocolBase:
		return connect.CodecNameJSON
	}
	return ""
}

// negotiateSubprotocol picks the first offered subprotocol this server both
// recognizes and can serve.
//
// A recognized token whose codec is missing is skipped rather than accepted,
// so a client offering JSON first still reaches a Protobuf-only server through
// its second choice.
func negotiateSubprotocol(
	requested []string,
	supported func(string) bool,
) (subprotocol, codecName string, outcome negotiationOutcome) {
	recognized := false
	for _, name := range requested {
		codec := codecForSubprotocol(name)
		if codec == "" {
			continue
		}
		recognized = true
		if supported(codec) {
			return name, codec, negotiationOK
		}
	}
	if recognized {
		return "", "", negotiationUnsupportedCodec
	}
	return "", "", negotiationUnrecognized
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

// registry is a name-keyed lookup for codecs and compressors.
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
