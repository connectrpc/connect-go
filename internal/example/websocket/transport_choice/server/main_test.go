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
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// newServer starts the same mux main() serves, on a real HTTP/1.1 listener: a
// WebSocket upgrade needs a hijackable connection, which an in-memory server
// does not give.
func newServer(tb testing.TB) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

func recordProtocol(into *string) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			stream, err := next(ctx, spec)
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				*into = info.Protocol
			}
			return stream, err
		}
	}
}

// One server, four client routing rules, one procedure: the same server stream
// comes back correctly every time, over whichever wire the client chose.
func TestClientChoosesTheTransport(t *testing.T) {
	httpServer := newServer(t)

	for _, test := range []struct {
		name     string
		selector connectwebsocket.Selector
		want     string
	}{
		{"default", nil, connectwebsocket.ProtocolConnectWebSocket},
		{"all", connectwebsocket.SelectAll, connectwebsocket.ProtocolConnectWebSocket},
		{"bidi only", connectwebsocket.SelectBidi, connect.ProtocolNameConnect},
		{"never", func(connect.Spec) bool { return false }, connect.ProtocolNameConnect},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := []connectwebsocket.ClientOption{
				connectwebsocket.WithHTTPClient(httpServer.Client()),
			}
			if test.selector != nil {
				options = append(options, connectwebsocket.WithSelector(test.selector))
			}
			transport, err := connectwebsocket.NewTransport(httpServer.URL, options...)
			if err != nil {
				t.Fatalf("websocket transport: %v", err)
			}
			var protocol string
			client := pingv1connect.NewPingServiceClient(
				connect.NewClient(transport, recordProtocol(&protocol)),
			)

			stream, err := client.CountUp(t.Context(), &v1.CountUpRequest{Number: 3})
			if err != nil {
				t.Fatalf("CountUp: %v", err)
			}
			var got []int64
			for {
				message, err := stream.Receive()
				if err != nil {
					break
				}
				got = append(got, message.Number)
			}
			if err := stream.Close(); err != nil {
				t.Fatalf("CountUp.Close: %v", err)
			}
			if len(got) != 3 || got[0] != 1 || got[2] != 3 {
				t.Errorf("got %v; want [1 2 3]", got)
			}
			if protocol != test.want {
				t.Errorf("CountUp used %q; want %q", protocol, test.want)
			}
		})
	}
}

// Unary is the other half of the default rule: it stays on HTTP unless the
// client asks for everything to be upgraded.
func TestUnaryFollowsTheSameRule(t *testing.T) {
	httpServer := newServer(t)

	for _, test := range []struct {
		name     string
		selector connectwebsocket.Selector
		want     string
	}{
		{"default leaves unary on HTTP", nil, connect.ProtocolNameConnect},
		{"SelectAll upgrades unary too", connectwebsocket.SelectAll, connectwebsocket.ProtocolConnectWebSocket},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := []connectwebsocket.ClientOption{
				connectwebsocket.WithHTTPClient(httpServer.Client()),
			}
			if test.selector != nil {
				options = append(options, connectwebsocket.WithSelector(test.selector))
			}
			transport, err := connectwebsocket.NewTransport(httpServer.URL, options...)
			if err != nil {
				t.Fatalf("websocket transport: %v", err)
			}
			var protocol string
			client := pingv1connect.NewPingServiceClient(
				connect.NewClient(transport, recordProtocol(&protocol)),
			)
			response, err := client.Ping(t.Context(), &v1.PingRequest{Number: 42})
			if err != nil {
				t.Fatalf("Ping: %v", err)
			}
			if response.Number != 42 {
				t.Errorf("got Number=%d; want 42", response.Number)
			}
			if protocol != test.want {
				t.Errorf("Ping used %q; want %q", protocol, test.want)
			}
		})
	}
}
