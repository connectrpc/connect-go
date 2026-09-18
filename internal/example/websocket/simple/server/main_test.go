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

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// newTestClient starts the same hybrid mux main() serves, on a real HTTP/1.1
// listener. httptest is used rather than an in-memory server because a
// WebSocket upgrade needs a hijackable connection.
func newTestClient(tb testing.TB, protocol *string) pingv1connect.PingServiceClient {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)

	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	if err != nil {
		tb.Fatalf("websocket transport: %v", err)
	}
	record := func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			stream, err := next(ctx, spec)
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				*protocol = info.Protocol
			}
			return stream, err
		}
	}
	return pingv1connect.NewPingServiceClient(connect.NewClient(transport, record))
}

func TestPingFallsThroughToHTTP(t *testing.T) {
	var protocol string
	client := newTestClient(t, &protocol)
	res, err := client.Ping(t.Context(), &v1.PingRequest{Number: 42, Text: "hello"})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if res.Number != 42 || res.Text != "hello" {
		t.Errorf("got Number=%d Text=%q; want 42 %q", res.Number, res.Text, "hello")
	}
	if protocol != connect.ProtocolNameConnect {
		t.Errorf("Ping used %q; want it to fall through to %q", protocol, connect.ProtocolNameConnect)
	}
}

func TestCumSumOverWebSocket(t *testing.T) {
	var protocol string
	client := newTestClient(t, &protocol)
	stream, err := client.CumSum(t.Context())
	if err != nil {
		t.Fatalf("CumSum: %v", err)
	}
	want := []int64{1, 3, 6, 10}
	for i, number := range []int64{1, 2, 3, 4} {
		if err := stream.Send(&v1.CumSumRequest{Number: number}); err != nil {
			t.Fatalf("CumSum.Send: %v", err)
		}
		msg, err := stream.Receive()
		if err != nil {
			t.Fatalf("CumSum.Receive: %v", err)
		}
		if msg.Sum != want[i] {
			t.Errorf("after %d messages got Sum=%d; want %d", i+1, msg.Sum, want[i])
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CumSum.CloseSend: %v", err)
	}
	if _, err := stream.Receive(); !errors.Is(err, io.EOF) {
		t.Errorf("after CloseSend got %v; want io.EOF", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("CumSum.Close: %v", err)
	}
	if protocol != connectwebsocket.ProtocolConnectWebSocket {
		t.Errorf("CumSum used %q; want %q", protocol, connectwebsocket.ProtocolConnectWebSocket)
	}
}
