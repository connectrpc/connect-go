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
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/wstransport"
)

// upgradeStatus sends a handshake by hand and returns the response status.
func upgradeStatus(tb testing.TB, url string, header http.Header) int {
	tb.Helper()
	request, err := http.NewRequestWithContext(tb.Context(), http.MethodGet, url, http.NoBody)
	assert.Nil(tb, err)
	request.Header = header.Clone()
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	response, err := http.DefaultClient.Do(request)
	assert.Nil(tb, err)
	_ = response.Body.Close()
	return response.StatusCode
}

func newMountedServer(tb testing.TB, options ...connecthttp.Option) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, options...)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// §4.2: an endpoint restricted to Connect over HTTP rejects a handshake with
// 400, whatever the handshake offered.
func TestHTTPOnlyEndpointRejectsUpgradeWithBadRequest(t *testing.T) {
	t.Parallel()
	httpServer := newMountedServer(t, connecthttp.WithoutWebSocket())
	for _, offer := range []string{"connectrpc.1+json", "unknown", ""} {
		header := http.Header{}
		if offer != "" {
			header.Set("Sec-WebSocket-Protocol", offer)
		}
		status := upgradeStatus(t, httpServer.URL+pingv1connect.PingServicePingProcedure, header)
		assert.Equal(t, status, http.StatusBadRequest, assert.Sprintf("offer %q", offer))
	}
}

// §4.4: a browser attaches cookies to a WebSocket handshake and no CORS
// preflight stands in the way, so a cross-origin one is refused with 403. That
// happens before the subprotocol is looked at, whatever it offered.
func TestCrossOriginIsRefusedBeforeNegotiation(t *testing.T) {
	t.Parallel()
	httpServer := newMountedServer(t)
	for _, offer := range []string{"connectrpc.1+json", "unknown"} {
		header := http.Header{}
		header.Set("Origin", "http://evil.example")
		header.Set("Sec-WebSocket-Protocol", offer)
		status := upgradeStatus(t, httpServer.URL+pingv1connect.PingServicePingProcedure, header)
		assert.Equal(t, status, http.StatusForbidden, assert.Sprintf("offer %q", offer))
	}
}

// §4.6: any frame from the peer is a sign of life, its own PINGs included. A
// client that pings but never answers the server's PINGs is not hung up on
// while its PINGs keep coming, and is once they stop.
func TestPeerPingsCountAsActivity(t *testing.T) {
	t.Parallel()
	httpServer := newKeepAliveServer(t,
		wstransport.WithKeepAlive(sparedInterval),
		wstransport.WithKeepAliveTimeout(sparedTimeout),
	)
	conn := openRawCumSum(t, httpServer)
	closed := make(chan int, 1)
	go func() {
		_, closeCode, _ := watchFrames(conn, nil)
		closed <- closeCode
	}()

	ctx, stopPinging := context.WithTimeout(t.Context(), 5*sparedTimeout)
	defer stopPinging()
	ticker := time.NewTicker(sparedInterval / 2)
	defer ticker.Stop()
pinging:
	for {
		select {
		case <-ctx.Done():
			break pinging
		case closeCode := <-closed:
			t.Fatalf("server closed with %d while the client was pinging", closeCode)
		case <-ticker.C:
			assert.Nil(t, writeClientFragment(conn, nil, true, opcodePing))
		}
	}

	select {
	case closeCode := <-closed:
		assert.Equal(t, closeCode, int(1011))
	case <-time.After(wsTestReadTimeout):
		t.Fatal("server never hung up on a silent client")
	}
}
