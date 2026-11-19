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
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// §4.4: a request with no Origin is not from a browser, and one whose origin
// names the request's own host is the ordinary same-origin call. Behind a
// TLS-terminating proxy the page's origin is https on a plaintext connection,
// so the scheme is not compared, whether it comes from there or from a header.
func TestSameOriginAndOriginlessUpgradesAreAllowed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		scheme         string
		forwardedProto string
	}{
		{name: "no Origin"},
		{name: "same origin", scheme: "http"},
		{name: "https origin on plaintext", scheme: "https"},
		{name: "https origin, proxy says http", scheme: "https", forwardedProto: "http"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer(t, pingServer{})
			request := newUpgradeRequest(t, httpServer.URL)
			if test.scheme != "" {
				request.Header.Set("Origin", test.scheme+"://"+request.URL.Host)
			}
			if test.forwardedProto != "" {
				request.Header.Set("X-Forwarded-Proto", test.forwardedProto)
			}
			res, err := httpServer.Client().Do(request)
			assert.Nil(t, err)
			t.Cleanup(func() { _ = res.Body.Close() })
			assert.Equal(t, res.StatusCode, http.StatusSwitchingProtocols)
		})
	}
}

// WithCheckOrigin must still be able to open the endpoint up.
func TestWithCheckOriginCanAllowCrossOrigin(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server,
		connecthttp.WithWebSocketCheckOrigin(func(*http.Request) bool { return true }),
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
