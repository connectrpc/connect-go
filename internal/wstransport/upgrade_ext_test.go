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

package wstransport_test

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/wstransport"
	"github.com/coder/websocket"
)

const pingProcedure = "/connect.ping.v1.PingService/CumSum"

// newUpgradeHandler builds the Upgrade handler alone, with no HTTP fallthrough
// behind it, so a rejected handshake is answered by Upgrade itself.
func newUpgradeHandler(tb testing.TB, options ...wstransport.ServerOption) http.Handler {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	return wstransport.Upgrade(server, nil, options...)
}

// hijackableRecorder satisfies http.Hijacker so a request reaches the checks
// that run after it. Hijack itself is never called: every test here is
// rejected before the handshake.
type hijackableRecorder struct {
	*httptest.ResponseRecorder
}

func (h hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("connectwebsocket_test: hijack not supported")
}

func newHijackableRecorder() hijackableRecorder {
	return hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
}

// upgradeRequest builds a syntactically valid WebSocket upgrade so each test
// can invalidate exactly one thing.
func upgradeRequest(tb testing.TB, subprotocol string) *http.Request {
	tb.Helper()
	req := httptest.NewRequestWithContext(tb.Context(), http.MethodGet, pingProcedure, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	if subprotocol != "" {
		req.Header.Set("Sec-WebSocket-Protocol", subprotocol)
	}
	return req
}

// httptest.ResponseRecorder does not implement http.Hijacker, which is exactly
// the h2c-only shape the handler must diagnose.
func TestUpgradeRejectsUnhijackableListener(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	newUpgradeHandler(t).ServeHTTP(recorder, upgradeRequest(t, "connectrpc.1"))
	assert.Equal(t, recorder.Code, http.StatusInternalServerError)
	// The message must name h2c: an operator who sees a bare 500 looks in the
	// wrong place.
	assert.True(t, len(recorder.Body.String()) > 0)
	assert.True(t, containsAll(recorder.Body.String(), "http.Hijacker", "h2c"))
}

func TestUpgradeRejectsDisallowedOrigin(t *testing.T) {
	t.Parallel()
	handler := newUpgradeHandler(t, wstransport.WithCheckOrigin(
		func(*http.Request) bool { return false },
	))
	recorder := newHijackableRecorder()
	handler.ServeHTTP(recorder, upgradeRequest(t, "connectrpc.1"))
	assert.Equal(t, recorder.Code, http.StatusForbidden)
}

func TestUpgradeRejectsMissingSubprotocol(t *testing.T) {
	t.Parallel()
	recorder := newHijackableRecorder()
	// No Sec-WebSocket-Protocol at all.
	newUpgradeHandler(t).ServeHTTP(recorder, upgradeRequest(t, ""))
	assert.Equal(t, recorder.Code, http.StatusBadRequest)
}

func TestUpgradeRejectsUnknownSubprotocol(t *testing.T) {
	t.Parallel()
	recorder := newHijackableRecorder()
	newUpgradeHandler(t).ServeHTTP(recorder, upgradeRequest(t, "chat, superchat"))
	assert.Equal(t, recorder.Code, http.StatusBadRequest)
}

// A subprotocol naming a codec the server does not have must fail before the
// handshake, not after.
func TestUpgradeRejectsUnsupportedCodec(t *testing.T) {
	t.Parallel()
	// Only the JSON codec is registered, so the +proto token has no codec.
	handler := newUpgradeHandler(t, wstransport.WithCodecs(connectproto.NewJSONCodec()))
	recorder := newHijackableRecorder()
	handler.ServeHTTP(recorder, upgradeRequest(t, "connectrpc.1+proto"))
	assert.Equal(t, recorder.Code, http.StatusUnsupportedMediaType)
}

// A request that is not an upgrade must fall through, not be rejected.
func TestUpgradeIgnoresNonWebSocketRequest(t *testing.T) {
	t.Parallel()
	recorder := newHijackableRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, pingProcedure, nil)
	newUpgradeHandler(t).ServeHTTP(recorder, req)
	// Mount passes nil for next, so a non-upgrade is a 404 rather than a 400:
	// the handler declined it instead of failing it.
	assert.Equal(t, recorder.Code, http.StatusNotFound)
}

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}

// WithSession still allows a Session of one's own, which is the reason the
// extension point exists.
func TestWithSessionReplacesTheDefault(t *testing.T) {
	t.Parallel()
	var served atomic.Int64
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	inner := wstransport.NewSession()
	counting := wstransport.SessionFunc(func(
		ctx context.Context,
		srv *connect.Server,
		conn *websocket.Conn,
		info wstransport.SessionInfo,
	) error {
		served.Add(1)
		return inner.Serve(ctx, srv, conn, info)
	})
	mux := http.NewServeMux()
	// WithSession has no public equivalent — replacing the serve loop would
	// put coder/websocket types in connecthttp's API — so this reaches the
	// internal option directly.
	mountBoth(mux, server, wstransport.WithSession(counting))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectStreaming),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 2}))
	response, err := stream.Receive()
	assert.Nil(t, err)
	assert.Equal(t, response.Sum, int64(2))
	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())
	assert.Equal(t, served.Load(), int64(1))
}

// logSink collects slog output from the session's own goroutine and says when
// a record has landed, so a reader never races the write.
type logSink struct {
	mu      sync.Mutex
	records []string
	wrote   chan struct{}
}

func newLogSink() *logSink {
	return &logSink{wrote: make(chan struct{}, 1)}
}

func (s *logSink) Write(record []byte) (int, error) {
	s.mu.Lock()
	s.records = append(s.records, string(record))
	s.mu.Unlock()
	select {
	case s.wrote <- struct{}{}:
	default:
	}
	return len(record), nil
}

func (s *logSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.records, "")
}

// A session that fails must say so where an operator will see it. Nothing else
// reports it: the peer already has its verdict, and Serve's error is dropped.
func TestSessionErrorIsLogged(t *testing.T) {
	t.Parallel()
	sink := newLogSink()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	failing := wstransport.SessionFunc(func(
		_ context.Context, _ *connect.Server, conn *websocket.Conn, _ wstransport.SessionInfo,
	) error {
		// Upgrade hands the connection to the Session and never takes it back,
		// so a Session that returns without closing strands the peer.
		_ = conn.CloseNow()
		return errors.New("session gave up")
	})
	mux := http.NewServeMux()
	mountBoth(mux, server,
		wstransport.WithSession(failing),
		wstransport.WithLogger(slog.New(slog.NewTextHandler(sink, nil))),
	)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectStreaming),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	_ = stream.Send(&pingv1.CumSumRequest{Number: 1})
	_, _ = stream.Receive()
	_ = stream.Close()

	select {
	case <-sink.wrote:
	case <-time.After(10 * time.Second):
		t.Fatal("the session error was never logged")
	}
	record := sink.text()
	assert.True(t, strings.Contains(record, "session gave up"))
	// The procedure and peer are what make one failing connection findable.
	assert.True(t, strings.Contains(record, pingv1connect.PingServiceCumSumProcedure))
	assert.True(t, strings.Contains(record, "peer="))
}

// A Session supplied through WithSession must be configured by the caller's
// options too. It holds none of its own — the limits arrive on SessionInfo —
// so there is no second place for them to live and disagree.
func TestWithSessionStillGetsTheOptions(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	mountBoth(mux, server,
		wstransport.WithReadMaxBytes(1024),
		// The explicit default: the shape that silently lost the limits
		// while the Session carried its own options.
		wstransport.WithSession(wstransport.NewSession()),
	)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
		connecthttp.WithSendMaxBytes(1<<20),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Text: strings.Repeat("x", 8192)})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
}

// The limits are on SessionInfo, so a custom Session can read them.
func TestSessionInfoCarriesTheLimits(t *testing.T) {
	t.Parallel()
	seen := make(chan wstransport.SessionInfo, 1)
	inner := wstransport.NewSession()
	recording := wstransport.SessionFunc(func(
		ctx context.Context,
		srv *connect.Server,
		conn *websocket.Conn,
		info wstransport.SessionInfo,
	) error {
		seen <- info
		return inner.Serve(ctx, srv, conn, info)
	})
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	// The limits go through connecthttp, which forwards them; the Session has
	// no public equivalent and is supplied directly.
	mountBoth(mux, server,
		wstransport.WithReadMaxBytes(4096),
		wstransport.WithSendMaxBytes(8192),
		wstransport.WithSession(recording),
	)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectStreaming),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	_, err = stream.Receive()
	assert.Nil(t, err)
	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())

	info := <-seen
	assert.Equal(t, info.ReadMaxBytes, 4096)
	assert.Equal(t, info.SendMaxBytes, 8192)
}

// The option types are the point of the split: an option meaningful to one
// side only must not compile against the other. These assertions fail at build
// time if a With* function is retyped into the wrong category.
var (
	// Shared options satisfy both.
	_ wstransport.ClientOption = wstransport.WithReadMaxBytes(0)
	_ wstransport.ServerOption = wstransport.WithReadMaxBytes(0)
	_ wstransport.ClientOption = wstransport.WithSendMaxBytes(0)
	_ wstransport.ServerOption = wstransport.WithSendMaxBytes(0)
	_ wstransport.ClientOption = wstransport.WithCodecs()
	_ wstransport.ServerOption = wstransport.WithCodecs()
	_ wstransport.ClientOption = wstransport.WithoutCompression()
	_ wstransport.ServerOption = wstransport.WithoutCompression()
	_ wstransport.ClientOption = wstransport.WithCompressMinBytes(0)
	_ wstransport.ServerOption = wstransport.WithCompressMinBytes(0)

	_ wstransport.ClientOption = wstransport.WithCompressors()
	_ wstransport.ServerOption = wstransport.WithCompressors()

	// Client-only: these have no server meaning, and WithHTTPClient reaching a
	// handler used to be silently ignored. The routing options moved to
	// connecthttp, which owns the choice between the two transports.
	_ wstransport.ClientOption = wstransport.WithHTTPClient(nil)
	_ wstransport.ClientOption = wstransport.WithHandshakeTimeout(0)
	_ wstransport.ClientOption = wstransport.WithSendCodec("")
	_ wstransport.ClientOption = wstransport.WithSendCompression("")
	_ wstransport.ClientOption = wstransport.WithEagerDial()

	// Server-only.
	_ wstransport.ServerOption = wstransport.WithCheckOrigin(nil)
	_ wstransport.ServerOption = wstransport.WithSession(nil)
	_ wstransport.ServerOption = wstransport.WithLogger(nil)
	_ wstransport.ClientOption = wstransport.WithPathPrefix("")
	_ wstransport.ServerOption = wstransport.WithPathPrefix("")
	// The monitoring hooks mirror each other, one per side.
	_ wstransport.ServerOption = wstransport.WithServerProtocolErrorHandler(nil)
	_ wstransport.ClientOption = wstransport.WithClientProtocolErrorHandler(nil)
)

// Mount alone must serve every procedure both ways. Which transport carries an
// RPC is the client's choice, so a server that registered only one would
// reject a client that chose the other — which is what the old selector-gated
// Mount did: a plain HTTP client got 404 on a streaming procedure.
func TestMountServesBothTransports(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	// A plain connecthttp client, which never upgrades.
	httpClient := pingv1connect.NewPingServiceClient(
		connect.NewClient(connecthttp.NewTransport(httpServer.Client(), httpServer.URL)))
	response, err := httpClient.Ping(t.Context(), &pingv1.PingRequest{Number: 3})
	assert.Nil(t, err)
	assert.Equal(t, response.Number, int64(3))

	// A WebSocket client with everything upgraded, including the unary call the
	// old Mount would not have registered for WebSocket at all.
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	websocketClient := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	response, err = websocketClient.Ping(t.Context(), &pingv1.PingRequest{Number: 4})
	assert.Nil(t, err)
	assert.Equal(t, response.Number, int64(4))

	stream, err := websocketClient.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 5}))
	sum, err := stream.Receive()
	assert.Nil(t, err)
	assert.Equal(t, sum.Sum, int64(5))
	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())
}

// One option configures both wires. The direction is what changed when the
// entry points moved: connecthttp owns Mount, so its own options reach the
// WebSocket half rather than the other way round.
func TestMountOptionsReachBothTransports(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, connecthttp.WithReadMaxBytes(1024))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	oversize := strings.Repeat("x", 8192)

	httpClient := pingv1connect.NewPingServiceClient(
		connect.NewClient(connecthttp.NewTransport(httpServer.Client(), httpServer.URL)))
	_, err := httpClient.Ping(t.Context(), &pingv1.PingRequest{Text: oversize})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)

	wsClient := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)))
	_, err = wsClient.Ping(t.Context(), &pingv1.PingRequest{Text: oversize})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
}
