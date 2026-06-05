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

package connect_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/generics/connect/ping/v1/pingv1connect"
)

// websocketSmokeServer is the minimal PingService implementation exercised by
// the WebSocket smoke tests. It mirrors the README/design-doc examples: a unary
// Ping echo and a CumSum bidi stream that emits a running total.
type websocketSmokeServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (websocketSmokeServer) Ping(
	_ context.Context,
	req *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {
	return connect.NewResponse(&pingv1.PingResponse{
		Number: req.Msg.GetNumber(),
		Text:   req.Msg.GetText(),
	}), nil
}

func (websocketSmokeServer) CumSum(
	_ context.Context,
	stream *connect.BidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse],
) error {
	var sum int64
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		sum += req.GetNumber()
		if err := stream.Send(&pingv1.CumSumResponse{Sum: sum}); err != nil {
			return err
		}
	}
}

// newWebSocketSmokeServer starts an HTTP/1.1 test server (WebSocket requires
// HTTP/1.1 and http.Hijacker, which httptest.NewServer provides) serving the
// PingService with the WebSocket transport enabled.
func newWebSocketSmokeServer(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(
		websocketSmokeServer{},
		connect.WithWebSocket(),
	))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

func TestWebSocketSmokeBidiCumSum(t *testing.T) {
	t.Parallel()
	url := newWebSocketSmokeServer(t)

	client := pingv1connect.NewPingServiceClient(
		http.DefaultClient, // ignored for the WebSocket dial
		url,                // http:// rewritten to ws:// by the transport
		connect.WithWebSocket(),
	)

	stream := client.CumSum(context.Background())
	inputs := []int64{1, 2, 3, 4}
	for _, number := range inputs {
		assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: number}))
	}
	assert.Nil(t, stream.CloseRequest())

	want := []int64{1, 3, 6, 10}
	got := make([]int64, 0, len(want))
	for {
		response, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		assert.Nil(t, err)
		got = append(got, response.GetSum())
	}
	assert.Nil(t, stream.CloseResponse())
	assert.Equal(t, got, want)
}

func TestWebSocketSmokeUnaryPing(t *testing.T) {
	t.Parallel()
	url := newWebSocketSmokeServer(t)

	client := pingv1connect.NewPingServiceClient(
		http.DefaultClient,
		url,
		connect.WithWebSocket(),
	)

	response, err := client.Ping(
		context.Background(),
		connect.NewRequest(&pingv1.PingRequest{Number: 42, Text: "hello"}),
	)
	assert.Nil(t, err)
	assert.Equal(t, response.Msg.GetNumber(), int64(42))
	assert.Equal(t, response.Msg.GetText(), "hello")
}

// newDualProtocolServer starts one server on a single listener that speaks both
// HTTP/1.1 (needed for the WebSocket upgrade) and cleartext HTTP/2 (h2c, used by
// the other Connect protocols). It returns the listener address and a function
// that reports the request.ProtoMajor observed for a given RPC procedure, so a
// test can prove which HTTP version actually carried each call.
func newDualProtocolServer(t *testing.T) (addr string, protoMajorFor func(procedure string) int) {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(
		websocketSmokeServer{},
		connect.WithWebSocket(),
	))

	var mu sync.Mutex
	seen := make(map[string]int)
	recorder := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		seen[request.URL.Path] = request.ProtoMajor
		mu.Unlock()
		mux.ServeHTTP(writer, request)
	})

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{
		Handler:           recorder,
		Protocols:         protocols,
		ReadHeaderTimeout: time.Second,
	}

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "localhost:0")
	assert.Nil(t, err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	return listener.Addr().String(), func(procedure string) int {
		mu.Lock()
		defer mu.Unlock()
		return seen[procedure]
	}
}

// TestWebSocketSmokeMixedHTTPVersions confirms that a single server can serve
// the WebSocket transport over HTTP/1.1 and the standard Connect protocol over
// HTTP/2 (h2c) at the same time.
func TestWebSocketSmokeMixedHTTPVersions(t *testing.T) {
	t.Parallel()
	addr, protoMajorFor := newDualProtocolServer(t)
	baseURL := "http://" + addr

	// HTTP/2 (h2c) client speaking the default Connect protocol. Forcing
	// UnencryptedHTTP2 (and disabling HTTP/1.1) on the transport guarantees the
	// Ping below travels over HTTP/2.
	h2cProtocols := new(http.Protocols)
	h2cProtocols.SetUnencryptedHTTP2(true)
	h2cClient := pingv1connect.NewPingServiceClient(
		&http.Client{Transport: &http.Transport{Protocols: h2cProtocols}},
		baseURL,
	)
	pong, err := h2cClient.Ping(
		context.Background(),
		connect.NewRequest(&pingv1.PingRequest{Number: 7}),
	)
	assert.Nil(t, err)
	assert.Equal(t, pong.Msg.GetNumber(), int64(7))

	// WebSocket client over the same server; the upgrade runs over HTTP/1.1.
	wsClient := pingv1connect.NewPingServiceClient(
		http.DefaultClient,
		baseURL,
		connect.WithWebSocket(),
	)
	stream := wsClient.CumSum(context.Background())
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 5}))
	assert.Nil(t, stream.CloseRequest())
	cumulative, err := stream.Receive()
	assert.Nil(t, err)
	assert.Equal(t, cumulative.GetSum(), int64(5))
	_, err = stream.Receive()
	assert.True(t, errors.Is(err, io.EOF))
	assert.Nil(t, stream.CloseResponse())

	// Prove the two RPCs really arrived over different HTTP versions on the one
	// server: the unary Ping over HTTP/2, the WebSocket upgrade over HTTP/1.1.
	assert.Equal(t, protoMajorFor(pingv1connect.PingServicePingProcedure), 2)
	assert.Equal(t, protoMajorFor(pingv1connect.PingServiceCumSumProcedure), 1)
}
