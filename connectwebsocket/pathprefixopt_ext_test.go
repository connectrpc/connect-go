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
	"sync"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

const testPathPrefix = "/ws"

// prefixRecorder notes the path and kind of every request that reaches it,
// which is what a load balancer would be routing on.
type prefixRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *prefixRecorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		kind := "http"
		if connectwebsocket.IsUpgrade(request) {
			kind = "upgrade"
		}
		r.mu.Lock()
		r.paths = append(r.paths, kind+" "+request.URL.Path)
		r.mu.Unlock()
		next.ServeHTTP(responseWriter, request)
	})
}

func (r *prefixRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func newWSPrefixServer(tb testing.TB) (*httptest.Server, *prefixRecorder) {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server, connectwebsocket.WithPathPrefix(testPathPrefix))
	recorder := &prefixRecorder{}
	httpServer := httptest.NewServer(recorder.wrap(mux))
	tb.Cleanup(httpServer.Close)
	return httpServer, recorder
}

// Every upgrade carries the prefix and every plain RPC does not, which is the
// split a load balancer routes on.
func TestPathPrefixSeparatesTheTransports(t *testing.T) {
	t.Parallel()
	httpServer, recorder := newWSPrefixServer(t)
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithPathPrefix(testPathPrefix),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	response, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 7})
	assert.Nil(t, err)
	assert.Equal(t, response.Number, int64(7))

	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 3}))
	sum, err := stream.Receive()
	assert.Nil(t, err)
	assert.Equal(t, sum.Sum, int64(3))
	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())

	assert.Equal(t, recorder.seen(), []string{
		"http " + pingv1connect.PingServicePingProcedure,
		"upgrade " + testPathPrefix + pingv1connect.PingServiceCumSumProcedure,
	})
}

// A plain request to a prefixed path is refused: the prefix means WebSocket, so
// a load balancer routing on it is never wrong.
func TestPathPrefixRequiresAnUpgrade(t *testing.T) {
	t.Parallel()
	httpServer, _ := newWSPrefixServer(t)

	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		httpServer.URL+testPathPrefix+pingv1connect.PingServiceCumSumProcedure,
		http.NoBody,
	)
	assert.Nil(t, err)
	request.Header.Set("Content-Type", "application/connect+proto")
	response, err := httpServer.Client().Do(request)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	assert.Equal(t, response.StatusCode, http.StatusUpgradeRequired)
	assert.Equal(t, response.Header.Get("Upgrade"), "websocket")
}

// With a prefix configured, the bare path stops accepting upgrades. Otherwise
// WebSocket traffic could still arrive without the prefix, and the split the
// load balancer depends on would not hold.
func TestPathPrefixRemovesUpgradeFromBarePaths(t *testing.T) {
	t.Parallel()
	httpServer, _ := newWSPrefixServer(t)

	// A client that does not know about the prefix dials the bare path.
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
	assert.NotNil(t, err)
}

// Plain HTTP still works at the bare paths, prefix or no prefix.
func TestPathPrefixLeavesHTTPAlone(t *testing.T) {
	t.Parallel()
	httpServer, recorder := newWSPrefixServer(t)
	client := pingv1connect.NewPingServiceClient(
		connect.NewClient(connecthttp.NewTransport(httpServer.Client(), httpServer.URL)))

	response, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 5})
	assert.Nil(t, err)
	assert.Equal(t, response.Number, int64(5))
	assert.Equal(t, recorder.seen(), []string{"http " + pingv1connect.PingServicePingProcedure})
}

// "ws", "/ws" and "/ws/" all name the same prefix, so the two ends cannot
// disagree over punctuation.
func TestPathPrefixIsNormalized(t *testing.T) {
	t.Parallel()
	for _, spelling := range []string{"ws", "/ws", "ws/", "/ws/"} {
		t.Run(spelling, func(t *testing.T) {
			t.Parallel()
			server := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(server, pingServer{})
			mux := http.NewServeMux()
			connectwebsocket.Mount(mux, server, connectwebsocket.WithPathPrefix(spelling))
			recorder := &prefixRecorder{}
			httpServer := httptest.NewServer(recorder.wrap(mux))
			t.Cleanup(httpServer.Close)

			transport, err := connectwebsocket.NewTransport(
				httpServer.URL,
				connectwebsocket.WithHTTPClient(httpServer.Client()),
				// Spelled differently at each end on purpose.
				connectwebsocket.WithPathPrefix("/ws/"),
			)
			assert.Nil(t, err)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
			stream, err := client.CumSum(t.Context())
			assert.Nil(t, err)
			t.Cleanup(func() { _ = stream.Close() })
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
			_, err = stream.Receive()
			assert.Nil(t, err)

			assert.Equal(t, recorder.seen(), []string{
				"upgrade /ws" + pingv1connect.PingServiceCumSumProcedure,
			})
		})
	}
}
