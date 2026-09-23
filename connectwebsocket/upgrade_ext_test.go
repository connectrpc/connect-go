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

package connectwebsocket_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
)

const pingProcedure = "/connect.ping.v1.PingService/CumSum"

// newUpgradeHandler builds the Upgrade handler alone, with no HTTP fallthrough
// behind it, so a rejected handshake is answered by Upgrade itself.
func newUpgradeHandler(tb testing.TB, options ...connectwebsocket.ServerOption) http.Handler {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	return connectwebsocket.Upgrade(server, nil, options...)
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
	newUpgradeHandler(t).ServeHTTP(recorder, upgradeRequest(t, "connect.v2"))
	assert.Equal(t, recorder.Code, http.StatusInternalServerError)
	// The message must name h2c: an operator who sees a bare 500 looks in the
	// wrong place.
	assert.True(t, len(recorder.Body.String()) > 0)
	assert.True(t, containsAll(recorder.Body.String(), "http.Hijacker", "h2c"))
}

func TestUpgradeRejectsDisallowedOrigin(t *testing.T) {
	t.Parallel()
	handler := newUpgradeHandler(t, connectwebsocket.WithCheckOrigin(
		func(*http.Request) bool { return false },
	))
	recorder := newHijackableRecorder()
	handler.ServeHTTP(recorder, upgradeRequest(t, "connect.v2"))
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
	handler := newUpgradeHandler(t, connectwebsocket.WithCodecs(connectproto.NewJSONCodec()))
	recorder := newHijackableRecorder()
	handler.ServeHTTP(recorder, upgradeRequest(t, "connect.v2+proto"))
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

// One option list has to configure both halves. The handshake and the message
// loop used to be configured separately — a Session built elsewhere kept its
// own defaults — so a server could refuse oversized handshakes while happily
// reading oversized messages, and nothing said so.
func TestOptionsReachTheMessageLoop(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(
		connectwebsocket.Mux(mux, server, connectwebsocket.WithReadMaxBytes(1024)),
		server,
	)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
		// The client must not refuse it first: the rejection has to come from
		// the server's read limit.
		connectwebsocket.WithSendMaxBytes(1<<20),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{Text: strings.Repeat("x", 8192)})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
}

// WithSession still allows a Session of one's own, which is the reason the
// extension point exists.
func TestWithSessionReplacesTheDefault(t *testing.T) {
	t.Parallel()
	var served atomic.Int64
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	inner := connectwebsocket.NewSession()
	counting := connectwebsocket.SessionFunc(func(
		ctx context.Context,
		srv *connect.Server,
		conn *websocket.Conn,
		info connectwebsocket.SessionInfo,
	) error {
		served.Add(1)
		return inner.Serve(ctx, srv, conn, info)
	})
	mux := http.NewServeMux()
	connecthttp.Mount(
		connectwebsocket.Mux(mux, server, connectwebsocket.WithSession(counting)),
		server,
	)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	assert.Nil(t, err)
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

// A Session supplied through WithSession must be configured by the caller's
// options too. It holds none of its own — the limits arrive on SessionInfo —
// so there is no second place for them to live and disagree.
func TestWithSessionStillGetsTheOptions(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(
		connectwebsocket.Mux(mux, server,
			connectwebsocket.WithReadMaxBytes(1024),
			// The explicit default: the shape that silently lost the limits
			// while the Session carried its own options.
			connectwebsocket.WithSession(connectwebsocket.NewSession()),
		),
		server,
	)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
		connectwebsocket.WithSendMaxBytes(1<<20),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{Text: strings.Repeat("x", 8192)})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
}

// The limits are on SessionInfo, so a custom Session can read them.
func TestSessionInfoCarriesTheLimits(t *testing.T) {
	t.Parallel()
	seen := make(chan connectwebsocket.SessionInfo, 1)
	inner := connectwebsocket.NewSession()
	recording := connectwebsocket.SessionFunc(func(
		ctx context.Context,
		srv *connect.Server,
		conn *websocket.Conn,
		info connectwebsocket.SessionInfo,
	) error {
		seen <- info
		return inner.Serve(ctx, srv, conn, info)
	})
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(
		connectwebsocket.Mux(mux, server,
			connectwebsocket.WithReadMaxBytes(4096),
			connectwebsocket.WithSendMaxBytes(8192),
			connectwebsocket.WithSession(recording),
		),
		server,
	)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	assert.Nil(t, err)
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
	_ connectwebsocket.ClientOption = connectwebsocket.WithReadMaxBytes(0)
	_ connectwebsocket.ServerOption = connectwebsocket.WithReadMaxBytes(0)
	_ connectwebsocket.ClientOption = connectwebsocket.WithSendMaxBytes(0)
	_ connectwebsocket.ServerOption = connectwebsocket.WithSendMaxBytes(0)
	_ connectwebsocket.ClientOption = connectwebsocket.WithCodecs()
	_ connectwebsocket.ServerOption = connectwebsocket.WithCodecs()
	_ connectwebsocket.ClientOption = connectwebsocket.WithoutCompression()
	_ connectwebsocket.ServerOption = connectwebsocket.WithoutCompression()
	_ connectwebsocket.ClientOption = connectwebsocket.WithCompressMinBytes(0)
	_ connectwebsocket.ServerOption = connectwebsocket.WithCompressMinBytes(0)

	_ connectwebsocket.ClientOption = connectwebsocket.WithCompressors()
	_ connectwebsocket.ServerOption = connectwebsocket.WithCompressors()

	// Client-only: these have no server meaning, and WithHTTPClient reaching a
	// handler used to be silently ignored. WithSelector is here because which
	// transport carries an RPC is the client's choice; a server accepts both.
	_ connectwebsocket.ClientOption = connectwebsocket.WithSelector(nil)
	_ connectwebsocket.ClientOption = connectwebsocket.WithHTTPClient(nil)
	_ connectwebsocket.ClientOption = connectwebsocket.WithHandshakeTimeout(0)
	_ connectwebsocket.ClientOption = connectwebsocket.WithSendCodec("")
	_ connectwebsocket.ClientOption = connectwebsocket.WithSendCompression("")
	_ connectwebsocket.ClientOption = connectwebsocket.WithFallbackOnUpgradeError()
	_ connectwebsocket.ClientOption = connectwebsocket.WithFallbackTransport(nil)

	// Server-only.
	_ connectwebsocket.ServerOption = connectwebsocket.WithCheckOrigin(nil)
	_ connectwebsocket.ServerOption = connectwebsocket.WithSession(nil)
	_ connectwebsocket.ServerOption = connectwebsocket.WithLogger(nil)
	_ connectwebsocket.ClientOption = connectwebsocket.WithPathPrefix("")
	_ connectwebsocket.ServerOption = connectwebsocket.WithPathPrefix("")
	_ connectwebsocket.ServerOption = connectwebsocket.WithHTTPOptions()
	// The monitoring hooks mirror each other, one per side.
	_ connectwebsocket.ServerOption = connectwebsocket.WithServerProtocolErrorHandler(nil)
	_ connectwebsocket.ClientOption = connectwebsocket.WithClientProtocolErrorHandler(nil)
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
	connectwebsocket.Mount(mux, server)
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
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(t, err)
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

// Options reach the HTTP handlers Mount builds, not just the WebSocket half.
func TestMountForwardsOptionsToHTTP(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server, connectwebsocket.WithReadMaxBytes(1024))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	httpClient := pingv1connect.NewPingServiceClient(
		connect.NewClient(connecthttp.NewTransport(httpServer.Client(), httpServer.URL)))
	_, err := httpClient.Ping(t.Context(), &pingv1.PingRequest{Text: strings.Repeat("x", 8192)})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
}
