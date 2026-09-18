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
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// pathRecorder records the path of every request that reaches it, keyed by
// whether it was a WebSocket upgrade. The two transports have to agree, and
// the only way to know is to watch both arrive.
type pathRecorder struct {
	mu        sync.Mutex
	websocket []string
	http      []string
}

func (r *pathRecorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		r.mu.Lock()
		if connectwebsocket.IsUpgrade(request) {
			r.websocket = append(r.websocket, request.URL.Path)
		} else {
			r.http = append(r.http, request.URL.Path)
		}
		r.mu.Unlock()
		next.ServeHTTP(responseWriter, request)
	})
}

func (r *pathRecorder) paths() (websocket, http []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.websocket...), append([]string(nil), r.http...)
}

// newPrefixedServer mounts both transports under prefix, the way a service
// sharing a host with other routes would.
func newPrefixedServer(tb testing.TB, prefix string) (*httptest.Server, *pathRecorder) {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	inner := http.NewServeMux()
	connectwebsocket.Mount(inner, server)
	recorder := &pathRecorder{}
	outer := http.NewServeMux()
	outer.Handle(prefix+"/", http.StripPrefix(prefix, recorder.wrap(inner)))
	httpServer := httptest.NewServer(outer)
	tb.Cleanup(httpServer.Close)
	return httpServer, recorder
}

// A base URL with a path prefix has to reach the same procedures a bare one
// does, over both halves of the transport.
func TestBaseURLWithPathPrefixRoutesBothTransports(t *testing.T) {
	t.Parallel()
	httpServer, recorder := newPrefixedServer(t, "/api")
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL+"/api",
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	// Unary travels over HTTP, bidi over WebSocket.
	res, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 7, Text: "prefixed"})
	assert.Nil(t, err)
	assert.Equal(t, res.Number, int64(7))
	assert.Equal(t, res.Text, "prefixed")

	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 3}))
	sum, err := stream.Receive()
	assert.Nil(t, err)
	assert.Equal(t, sum.Sum, int64(3))
	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())

	// StripPrefix has already run, so the recorded paths are the bare
	// procedures: what matters is that the prefix was there to strip.
	websocketPaths, httpPaths := recorder.paths()
	assert.Equal(t, websocketPaths, []string{pingv1connect.PingServiceCumSumProcedure})
	assert.Equal(t, httpPaths, []string{pingv1connect.PingServicePingProcedure})
}

// Whatever the shape of the base URL's path, both halves must derive the same
// request path from it. connecthttp joins with its own helper and this package
// with another; a divergence would quietly route the two halves apart.
func TestBothTransportsAgreeOnTheRequestPath(t *testing.T) {
	t.Parallel()
	for _, suffix := range []string{"", "/", "/api", "/api/", "/api//", "/a/b"} {
		t.Run("base"+suffix, func(t *testing.T) {
			t.Parallel()
			recorder := &pathRecorder{}
			// Nothing is mounted: every request 404s. The paths are recorded on
			// the way in, which is all this test reads.
			httpServer := httptest.NewServer(recorder.wrap(http.NotFoundHandler()))
			t.Cleanup(httpServer.Close)

			transport, err := connectwebsocket.NewTransport(
				httpServer.URL+suffix,
				connectwebsocket.WithHTTPClient(httpServer.Client()),
			)
			assert.Nil(t, err)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

			_, err = client.Ping(t.Context(), &pingv1.PingRequest{})
			assert.NotNil(t, err)
			stream, err := client.CumSum(t.Context())
			assert.Nil(t, err)
			assert.NotNil(t, stream.Send(&pingv1.CumSumRequest{}))
			_ = stream.Close()

			websocketPaths, httpPaths := recorder.paths()
			assert.Equal(t, len(websocketPaths), 1)
			assert.Equal(t, len(httpPaths), 1)
			assert.Equal(t,
				strings.TrimSuffix(websocketPaths[0], pingv1connect.PingServiceCumSumProcedure),
				strings.TrimSuffix(httpPaths[0], pingv1connect.PingServicePingProcedure),
			)
			// And the shared prefix is the base URL's own path, normalized.
			assert.Equal(t, websocketPaths[0], strings.TrimSuffix(suffix, "/")+pingv1connect.PingServiceCumSumProcedure)
		})
	}
}

// A path prefix that the client adds and the server does not strip shows up as
// a procedure the registry has never heard of. The session has to answer that,
// because the handshake has already succeeded and a caller waiting on Receive
// has nothing else to wait for.
func TestUnregisteredProcedureIsUnimplemented(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(t, err)

	stream, err := transport.NewClientStream(t.Context(), connect.Spec{
		Procedure:  "/connect.ping.v1.PingService/NoSuchMethod",
		StreamType: connect.StreamTypeBidi,
	})
	assert.Nil(t, err)
	t.Cleanup(func() { _ = stream.Close() })
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	err = stream.Receive(&pingv1.CumSumResponse{})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnimplemented)
}
