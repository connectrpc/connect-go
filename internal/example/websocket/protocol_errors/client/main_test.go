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
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/example/websocket/protocol_errors/misframe"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// The mirror: a server that misframes its responses is reported to the
// client's handler, which is the only way a client can tell a framing fault
// from any other InvalidArgument.
func TestClientHandlerSeesAMisframingServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.PingServiceCumSumProcedure, http.HandlerFunc(misframe.Handler))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	var (
		gotFault     connectwebsocket.ProtocolFault
		gotProcedure string
		calls        int
	)
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithClientProtocolErrorHandler(
			func(spec connect.Spec, fault connectwebsocket.ProtocolFault, _ *connect.Error) {
				calls++
				gotFault = fault
				gotProcedure = spec.Procedure
			},
		),
	)
	if err != nil {
		t.Fatalf("websocket transport: %v", err)
	}
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	stream, err := client.CumSum(t.Context())
	if err != nil {
		t.Fatalf("CumSum: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	if err := stream.Send(&v1.CumSumRequest{Number: 7}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := stream.Receive(); err == nil {
		t.Fatal("Receive succeeded against a misframing server; want a framing error")
	}

	if calls != 1 {
		t.Errorf("handler called %d times; want 1", calls)
	}
	if gotFault != connectwebsocket.FaultMarker {
		t.Errorf("got fault %s; want %s", gotFault, connectwebsocket.FaultMarker)
	}
	if gotProcedure != pingv1connect.PingServiceCumSumProcedure {
		t.Errorf("got procedure %q; want %q", gotProcedure, pingv1connect.PingServiceCumSumProcedure)
	}
}
