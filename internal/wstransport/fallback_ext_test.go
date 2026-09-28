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
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// newHTTPOnlyServer serves Connect over HTTP with the WebSocket transport
// turned off, so an upgrade attempt fails while ordinary RPCs succeed. That is
// exactly the situation WithFallbackOnUpgradeError exists for: a peer that
// does not speak this transport.
//
// WithoutWebSocket is load-bearing here. Mount registers both transports by
// default, so without it this server would accept the upgrade and the tests
// below would pass without ever reaching the fallback.
func newHTTPOnlyServer(tb testing.TB) (*httptest.Server, *atomic.Int64) {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, connecthttp.WithoutWebSocket())
	var rpcs atomic.Int64
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only a real RPC counts; the refused upgrade is a GET.
		if r.Method == http.MethodPost {
			rpcs.Add(1)
		}
		mux.ServeHTTP(w, r)
	})
	httpServer := httptest.NewServer(counted)
	tb.Cleanup(httpServer.Close)
	return httpServer, &rpcs
}

func TestFallbackOnUpgradeErrorRetriesOverHTTP(t *testing.T) {
	t.Parallel()
	httpServer, _ := newHTTPOnlyServer(t)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
		connecthttp.WithFallbackOnUpgradeError(),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	res, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 9, Text: "hi"})
	assert.Nil(t, err)
	assert.Equal(t, res.Number, int64(9))
	assert.Equal(t, res.Text, "hi")
}

// The same server without the option must fail, so the test above cannot pass
// for some reason other than the retry.
func TestWithoutFallbackOnUpgradeErrorTheCallFails(t *testing.T) {
	t.Parallel()
	httpServer, _ := newHTTPOnlyServer(t)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 9})
	assert.NotNil(t, err)
}

// A canceled call must not be retried on the fallback: the peer did nothing
// wrong, the caller gave up, and a retry would be work nobody asked for.
func TestFallbackOnUpgradeErrorSkipsRetryWhenCanceled(t *testing.T) {
	t.Parallel()
	httpServer, rpcs := newHTTPOnlyServer(t)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
		connecthttp.WithFallbackOnUpgradeError(),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 1})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
	// The count is the assertion: no RPC reached the HTTP half.
	assert.Equal(t, rpcs.Load(), int64(0))
}

// The retry reaches the HTTP half exactly once, rather than the call happening
// to succeed for some other reason.
func TestFallbackOnUpgradeErrorReachesTheHTTPHalfOnce(t *testing.T) {
	t.Parallel()
	httpServer, rpcs := newHTTPOnlyServer(t)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
		connecthttp.WithFallbackOnUpgradeError(),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
	assert.Nil(t, err)
	assert.Equal(t, rpcs.Load(), int64(1))
}
