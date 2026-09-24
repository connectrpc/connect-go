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
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// A browser attaches the user's cookies to a WebSocket handshake and no CORS
// preflight stands in the way, so a cross-origin upgrade must be refused
// unless the server opted in.
func TestCrossOriginUpgradeIsRefusedByDefault(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	request := newUpgradeRequest(t, httpServer.URL)
	request.Header.Set("Origin", "https://evil.example.com")
	res, err := httpServer.Client().Do(request)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	assert.Equal(t, res.StatusCode, http.StatusForbidden)
}

// A request with no Origin header is not a browser, and must still be served.
func TestUpgradeWithoutOriginIsAllowed(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	request := newUpgradeRequest(t, httpServer.URL)
	res, err := httpServer.Client().Do(request)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	assert.Equal(t, res.StatusCode, http.StatusSwitchingProtocols)
}

// An origin matching the host is the ordinary same-origin browser call.
func TestSameOriginUpgradeIsAllowed(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	request := newUpgradeRequest(t, httpServer.URL)
	request.Header.Set("Origin", httpServer.URL)
	res, err := httpServer.Client().Do(request)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	assert.Equal(t, res.StatusCode, http.StatusSwitchingProtocols)
}

// WithCheckOrigin must still be able to open the endpoint up.
func TestWithCheckOriginCanAllowCrossOrigin(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(
		connectwebsocket.Mux(mux, server,
			connectwebsocket.WithCheckOrigin(func(*http.Request) bool { return true })),
		server,
	)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	request := newUpgradeRequest(t, httpServer.URL)
	request.Header.Set("Origin", "https://evil.example.com")
	res, err := httpServer.Client().Do(request)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	assert.Equal(t, res.StatusCode, http.StatusSwitchingProtocols)
}

// newUpgradeRequest builds a handshake by hand so that a test can set headers
// the WebSocket client would not let it set.
func newUpgradeRequest(tb testing.TB, baseURL string) *http.Request {
	tb.Helper()
	request, err := http.NewRequestWithContext(
		tb.Context(),
		http.MethodGet,
		baseURL+pingv1connect.PingServiceCumSumProcedure,
		nil,
	)
	assert.Nil(tb, err)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Sec-WebSocket-Protocol", "connectrpc.1+proto")
	return request
}
