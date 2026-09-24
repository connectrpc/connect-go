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
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/coder/websocket"
)

// NewTransport returns a [connect.Transport] that carries streaming RPCs over
// WebSocket and everything else over HTTP, both against baseURL. An http or
// https scheme is rewritten to ws or wss for the WebSocket half.
//
// The HTTP half is a [connecthttp] transport built here, so one set of options
// configures both and neither can drift from the other. Replace it with
// [WithFallbackTransport], or route every RPC over WebSocket with
// WithSelector([SelectAll]).
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
	codecs, codecErr := newCodecPair(opts.codecs)
	if codecErr != nil {
		return nil, codecErr
	}
	if _, ok := newCodecRegistry(opts.codecs).get(opts.sendCodec); !ok {
		return nil, connect.Errorf(connect.CodeUnknown, "unknown codec %q", opts.sendCodec)
	}
	websocketTransport := &clientTransport{
		baseURL:    parsed,
		httpClient: opts.httpClient,
		codecs:     codecs,
		bodyIsText: opts.sendCodec == connect.CodecNameJSON,
		opts:       &opts,
	}
	fallback := opts.fallbackTransport
	if fallback == nil {
		fallback = connecthttp.NewTransport(opts.httpClient, baseURL, httpOptions(&opts)...)
	}
	return newHybridTransport(websocketTransport, fallback, &opts), nil
}

// httpOptions forwards the options both transports share, so a limit or codec
// set once applies whichever way an RPC travels. Options with no HTTP meaning
// are not forwarded, and HTTP-only options are reached through
// [WithFallbackTransport].
func httpOptions(opts *options) []connecthttp.Option {
	forwarded := []connecthttp.Option{
		connecthttp.WithCodecs(opts.codecs...),
		connecthttp.WithCompressors(opts.compressors...),
		connecthttp.WithSendCodec(opts.sendCodec),
		connecthttp.WithCompressMinBytes(opts.compressMinBytes),
		connecthttp.WithReadMaxBytes(opts.readMaxBytes),
		connecthttp.WithSendMaxBytes(opts.sendMaxBytes),
	}
	if opts.sendCompression != "" {
		forwarded = append(forwarded, connecthttp.WithSendCompression(opts.sendCompression))
	}
	return forwarded
}

type clientTransport struct {
	baseURL    *url.URL
	httpClient *http.Client
	// Both codecs are live on every connection: the frame type selects between
	// them per message, so a peer may answer in either. bodyIsText is the
	// negotiated default for what this client sends.
	codecs     codecPair
	bodyIsText bool
	opts       *options
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

	// The caller's metadata travels in the M message and nowhere else. Copying
	// it onto the handshake as well would put the same keys on two channels
	// where the handshake wins, so a client's own M would be shadowed by its
	// own headers — redundant at best, and confusing the moment the two
	// disagree. The handshake's headers are for what the connection carries,
	// not for what the RPC says.
	requestMeta := make(http.Header, 8)
	if info != nil {
		for key, values := range info.RequestHeader().All() {
			requestMeta[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
		}
	}
	dialURL := *t.baseURL
	// The prefix goes on the WebSocket half only: the HTTP fallback keeps the
	// bare procedure paths, which is what makes the two distinguishable to a
	// load balancer. See WithPathPrefix.
	dialURL.Path = strings.TrimSuffix(dialURL.Path, "/") + t.opts.pathPrefix + spec.Procedure
	if encoded, ok := encodeTimeout(ctx); ok {
		query := dialURL.Query()
		query.Set(wsQueryTimeoutMs, encoded)
		dialURL.RawQuery = query.Encode()
	}

	call := &wsClientCall{
		ctx: ctx,
		dialOptions: &websocket.DialOptions{
			HTTPClient: t.httpClient,
			// The subprotocol names both the transport and the codec, so it
			// has to follow WithSendCodec: offering the wrong token would have
			// the server decode with a codec the client is not encoding with.
			Subprotocols: []string{subprotocolForCodec(t.opts.sendCodec)},
			// Offer permessage-deflate with a per-message context. A server
			// that declines leaves messages uncompressed; one that requires
			// no-context-takeover is already satisfied by this offer.
			CompressionMode:      t.opts.compressionMode(),
			CompressionThreshold: t.opts.compressionThreshold(),
		},
		url:              &dialURL,
		subprotocol:      subprotocolForCodec(t.opts.sendCodec),
		handshakeTimeout: t.opts.handshakeTimeout,
		dialDone:         make(chan struct{}),
		readLimit:        messageReadLimit(t.opts.readMaxBytes),
	}
	conn := &websocketClientConn{
		call:   call,
		codecs: t.codecs,
		marshaler: messageWriter{
			// No compression here: permessage-deflate compresses whole messages
			// below this layer.
			ctx:          ctx,
			sender:       call,
			codecs:       t.codecs,
			bodyIsText:   t.bodyIsText,
			sendMaxBytes: t.opts.sendMaxBytes,
		},
		unmarshaler: websocketClientUnmarshaler{
			call:            call,
			codecs:          t.codecs,
			readMaxBytes:    t.opts.readMaxBytes,
			spec:            spec,
			onProtocolError: t.opts.onClientProtocolError,
		},
		requestMeta:     requestMeta,
		responseHeader:  make(http.Header),
		responseTrailer: make(http.Header),
	}
	if info != nil {
		info.PeerAddr = dialURL.Host
		info.Protocol = ProtocolConnectWebSocket
		info.Codec = t.opts.sendCodec
		info.RequestEncoding = connect.CompressionNameIdentity
	}
	if t.opts.fallbackOnUpgradeError {
		// Dial now rather than on first use. A caller who asked to fall back on
		// a failed handshake needs the answer here, while there is still
		// somewhere to fall back to: once this returns a stream, the upgrade
		// error would surface from Send and the choice would be gone.
		if err := call.ensureDialed(); err != nil {
			return nil, err
		}
	}
	return &clientStream{
		conn: conn,
		info: info,
		// Unary and client-streaming both have exactly one response message,
		// and it is the second read that reaches the trailers.
		singleResponse: spec.StreamType&connect.StreamTypeServer == 0,
	}, nil
}

// clientStream adapts [websocketClientConn] to [connect.ClientStream].
type clientStream struct {
	conn           *websocketClientConn
	info           *connect.CallInfo
	singleResponse bool
	// Close may run concurrently with Receive by contract, and both publish.
	// A sync.Once rather than a flag: the ordering that makes a plain bool
	// safe today is incidental — Close publishes before touching the conn and
	// Receive after — and moving either call would break it silently.
	publishOnce        sync.Once
	publishHeadersOnce sync.Once
}

// SendHeaders dials and writes the M message that opens the
// stream, which is what actually transmits the request metadata.
func (s *clientStream) SendHeaders() error {
	if err := s.conn.call.ensureDialed(); err != nil {
		return err
	}
	if err := s.conn.open(); err != nil {
		return err
	}
	return nil // literal nil; a nil *connect.Error is a non-nil error
}

func (s *clientStream) Send(msg any) error {
	return s.conn.Send(msg)
}

func (s *clientStream) CloseSend() error {
	return s.conn.CloseRequest()
}

func (s *clientStream) Receive(msg any) error {
	err := s.conn.Receive(msg)
	if err != nil {
		// Reading to completion is what makes response metadata available.
		s.publishMetadata()
		return err
	}
	// Leading metadata has arrived by now, and connecthttp makes it readable at
	// exactly this point: after the first Receive, not before.
	s.publishHeadersOnce.Do(s.publishHeaders)
	if s.singleResponse {
		// Such a caller receives exactly once, so nothing would ever read the
		// end-of-stream message — and the trailers it carries would be lost. A
		// server-streaming caller reaches it by reading to io.EOF.
		endErr := s.conn.Receive(nil)
		s.publishMetadata()
		if endErr != nil && !errors.Is(endErr, io.EOF) {
			return endErr
		}
	}
	return nil
}

func (s *clientStream) Close() error {
	s.publishMetadata()
	_ = s.conn.CloseRequest()
	return s.conn.CloseResponse()
}

// publishMetadata copies the response metadata onto the call's [connect.CallInfo],
// where callers read it. It runs once.
//
// The write happens during Receive, so a caller reading CallInfo from another
// goroutine while receiving on the same stream races it. That is not specific
// to this transport — connecthttp writes the same object from inside its own
// Receive — so it is left to be fixed where connect.Header is defined rather
// than papered over here; see "Current limitations" in
// docs/websocket-transport-design.md.
func (s *clientStream) publishMetadata() {
	if s.info == nil {
		return
	}
	s.publishOnce.Do(s.publish)
}

func (s *clientStream) publishHeaders() {
	if s.info == nil {
		return
	}
	for key, values := range s.conn.ResponseHeader() {
		s.info.ResponseHeader().SetValues(key, values)
	}
}

func (s *clientStream) publish() {
	s.publishHeadersOnce.Do(s.publishHeaders)
	for key, values := range s.conn.ResponseTrailer() {
		s.info.ResponseTrailer().SetValues(key, values)
	}
	// Identity at the Connect layer: permessage-deflate is below the protocol.
	s.info.ResponseEncoding = connect.CompressionNameIdentity
}
