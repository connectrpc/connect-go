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
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// countingTransport records whether the fallback was reached, which is the
// only way to tell "retried on HTTP" from "the WebSocket half happened to
// work".
type countingTransport struct {
	inner connect.Transport
	calls atomic.Int64
}

func (t *countingTransport) NewClientStream(
	ctx context.Context,
	spec connect.Spec,
) (connect.ClientStream, error) {
	t.calls.Add(1)
	return t.inner.NewClientStream(ctx, spec)
}

// newHTTPOnlyServer serves Connect over HTTP but mounts no WebSocket handler,
// so an upgrade attempt fails while ordinary RPCs succeed. That is exactly the
// situation WithFallbackOnUpgradeError exists for: a peer that does not speak
// this transport.
func newHTTPOnlyServer(tb testing.TB) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

func TestFallbackOnUpgradeErrorRetriesOverHTTP(t *testing.T) {
	t.Parallel()
	httpServer := newHTTPOnlyServer(t)
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
		connectwebsocket.WithFallbackOnUpgradeError(),
	)
	assert.Nil(t, err)
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
	httpServer := newHTTPOnlyServer(t)
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{Number: 9})
	assert.NotNil(t, err)
}

// A canceled call must not be retried on the fallback: the peer did nothing
// wrong, the caller gave up, and a retry would be work nobody asked for.
func TestFallbackOnUpgradeErrorSkipsRetryWhenCanceled(t *testing.T) {
	t.Parallel()
	httpServer := newHTTPOnlyServer(t)
	fallback := &countingTransport{
		inner: connecthttp.NewTransport(httpServer.Client(), httpServer.URL),
	}
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
		connectwebsocket.WithFallbackOnUpgradeError(),
		connectwebsocket.WithFallbackTransport(fallback),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.Ping(ctx, &pingv1.PingRequest{Number: 1})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
	assert.Equal(t, fallback.calls.Load(), int64(0))
}

// WithFallbackTransport must replace the transport NewTransport would have
// built, not sit alongside it.
func TestWithFallbackTransportReplacesTheDefault(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	fallback := &countingTransport{
		inner: connecthttp.NewTransport(httpServer.Client(), httpServer.URL),
	}
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithFallbackTransport(fallback),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	// Unary is routed to the fallback by the default selector.
	res, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 4})
	assert.Nil(t, err)
	assert.Equal(t, res.Number, int64(4))
	assert.Equal(t, fallback.calls.Load(), int64(1))

	// Streaming still goes over WebSocket, so the fallback count must not move.
	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 2}))
	_, err = stream.Receive()
	assert.Nil(t, err)
	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())
	assert.Equal(t, fallback.calls.Load(), int64(1))
}

// When the retry is taken and the fallback is itself broken, the call fails —
// but the caller hears only the fallback's error. The fallback transport is
// lazy too, so its NewClientStream succeeds and the WebSocket error is
// discarded before its own failure surfaces from Send.
func TestFallbackOnUpgradeErrorRetriesIntoABrokenFallback(t *testing.T) {
	t.Parallel()
	httpServer := newHTTPOnlyServer(t)
	broken := &countingTransport{inner: connecthttp.NewTransport(
		httpServer.Client(),
		"http://127.0.0.1:1", // nothing listens here
	)}
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
		connectwebsocket.WithFallbackOnUpgradeError(),
		connectwebsocket.WithFallbackTransport(broken),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
	assert.NotNil(t, err)
	// What matters is that the retry happened at all: the WebSocket half failed
	// and the fallback was reached exactly once.
	assert.Equal(t, broken.calls.Load(), int64(1))
}
