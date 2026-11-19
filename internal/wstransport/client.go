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
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/coder/websocket"
)

// NewTransport returns a [connect.Transport] that carries every RPC over
// WebSocket, with no HTTP fallback. An http or https scheme in baseURL is
// rewritten to ws or wss.
//
// Do not call this directly. It exists for
// [connectrpc.com/connect/v2/connecthttp], which builds one of these and wraps
// it with an HTTP transport to route each RPC by a selector. Use
// connecthttp.NewTransport with connecthttp.WithWebSocket instead; a transport
// from here upgrades everything and fails against a peer that does not.
func NewTransport(baseURL string, options ...ClientOption) (connect.Transport, error) {
	opts := defaultOptions()
	for _, opt := range options {
		opt.applyToClient(&opts)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, connect.Errorf(connect.CodeUnavailable, "invalid base URL: %s", err).WithCause(err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "ws", "":
		parsed.Scheme = "ws"
	case "https", "wss":
		parsed.Scheme = "wss"
	default:
		return nil, connect.Errorf(connect.CodeUnavailable, "unsupported URL scheme %q", parsed.Scheme)
	}
	codecs := newCodecRegistry(opts.codecs)
	sendCodec, ok := codecs.get(opts.sendCodec)
	if !ok {
		return nil, connect.Errorf(connect.CodeUnknown, "unknown codec %q", opts.sendCodec)
	}
	if !validCodecName(opts.sendCodec) {
		return nil, connect.Errorf(
			connect.CodeUnknown,
			"send codec %q has no WebSocket subprotocol; §4.3 requires a lower-case HTTP token",
			opts.sendCodec,
		)
	}
	offered := []string{subprotocolForCodec(opts.sendCodec)}
	for _, codec := range carriedCodecs(opts.codecs) {
		if codec.Name() != opts.sendCodec {
			offered = append(offered, subprotocolForCodec(codec.Name()))
		}
	}
	return &clientTransport{
		baseURL:    parsed,
		httpClient: opts.httpClient,
		sendCodec:  sendCodec,
		dial: &dialConfig{
			offered:           offered,
			codecs:            codecs,
			handshakeTimeout:  opts.handshakeTimeout,
			readLimit:         messageReadLimit(opts.readMaxBytes),
			keepAliveInterval: opts.keepAliveInterval,
			keepAliveTimeout:  opts.keepAliveTimeout,
		},
		opts: &opts,
	}, nil
}

type clientTransport struct {
	baseURL    *url.URL
	httpClient *http.Client
	sendCodec  connect.Codec
	dial       *dialConfig
	opts       *options
}

// offeredSubprotocols returns a copy of the tokens to offer, for a call's dial
// options to keep without sharing the transport's slice.
func (t *clientTransport) offeredSubprotocols() []string {
	return slices.Clone(t.dial.offered)
}

// encodeTimeout renders the context's deadline for the connect-timeout-ms
// query parameter. The query string is the deadline's only channel: a browser
// cannot set a header on a handshake, so a header would serve no client this
// transport has to interoperate with.
func encodeTimeout(ctx context.Context) (string, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return "", false
	}
	// Rounded up, never down. Truncating can advertise a deadline shorter than
	// the caller's own, and a server that gives up first is killed mid-read by
	// the WebSocket library — which closes the connection, so the deadline
	// error never reaches the client and it reports the close as Unavailable
	// instead.
	millis := int64((time.Until(deadline) + time.Millisecond - 1) / time.Millisecond)
	// A non-positive or absurdly distant deadline is not expressible; let the
	// context alone enforce it rather than sending nonsense.
	encoded := strconv.FormatInt(millis, 10)
	if millis <= 0 || len(encoded) > 10 {
		return "", false
	}
	return encoded, true
}

func (t *clientTransport) NewClientStream(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
	info, _ := connect.CallInfoForClientContext(ctx)
	dialURL := t.dialURL(ctx, spec)
	call := t.newCall(dialURL)
	conn := &websocketClientConn{
		ctx: ctx,
		frameDecoder: frameDecoder{
			fromServer:   true,
			readMaxBytes: t.opts.readMaxBytes,
		},
		call: call,
		marshaler: messageWriter{
			// No compression here: permessage-deflate compresses whole messages
			// below this layer.
			sender:       call,
			sendMaxBytes: t.opts.sendMaxBytes,
		},
		spec:            spec,
		onProtocolError: t.opts.onClientProtocolError,
		callInfo:        info,
		responseHeader:  make(http.Header),
		responseTrailer: make(http.Header),
	}
	if info != nil {
		info.Spec = spec
		conn.marshaler.stats = &info.SendStats
		conn.receiveStats = &info.ReceiveStats
		info.PeerAddr = dialURL.Host
		info.Protocol = ProtocolConnectWebSocket
		info.Codec = t.opts.sendCodec
		info.RequestEncoding = connect.CompressionNameIdentity
	}
	if t.opts.eagerDial {
		// Dial now rather than on first use. A caller who asked for this has
		// somewhere else to send the RPC and needs the answer here, while that
		// choice is still open: once this returns a stream, the upgrade error
		// would surface from Send and the choice would be gone.
		if err := call.ensureDialed(ctx); err != nil {
			return nil, err
		}
	}
	return &clientStream{
		conn: conn,
		// Unary and client-streaming both have exactly one response message,
		// and it is the second read that reaches the trailers.
		singleResponse: spec.StreamType&connect.StreamTypeServer == 0,
	}, nil
}

// dialURL is where spec's procedure is served, carrying ctx's deadline.
func (t *clientTransport) dialURL(ctx context.Context, spec connect.Spec) *url.URL {
	dialURL := *t.baseURL
	dialURL.Path = strings.TrimSuffix(dialURL.Path, "/") + spec.Procedure
	if encoded, ok := encodeTimeout(ctx); ok {
		query := dialURL.Query()
		query.Set(wsQueryTimeoutMs, encoded)
		dialURL.RawQuery = query.Encode()
	}
	return &dialURL
}

// newCall prepares, but does not dial, the connection for one RPC.
func (t *clientTransport) newCall(dialURL *url.URL) *wsClientCall {
	drain := newDrainLimiter(t.dial.readLimit)
	beat := &heartbeat{}
	return &wsClientCall{
		heartbeat: beat,
		config:    t.dial,
		drain:     drain,
		dialOptions: &websocket.DialOptions{
			HTTPClient: withDrainLimiter(t.httpClient, drain),
			// §4.6: a server's own PINGs and PONGs are frames from it too.
			OnPingReceived: beat.onPing,
			OnPongReceived: beat.onPong,
			// §4.3: the preferred codec first, then any other this client can
			// encode. The server picks one and the client follows its choice.
			Subprotocols: t.offeredSubprotocols(),
			// Offer permessage-deflate with a per-message context. A server
			// that declines leaves messages uncompressed; one that requires
			// no-context-takeover is already satisfied by this offer.
			CompressionMode:      t.opts.compressionMode(),
			CompressionThreshold: t.opts.compressionThreshold(),
		},
		url:   dialURL,
		codec: t.sendCodec,
	}
}
