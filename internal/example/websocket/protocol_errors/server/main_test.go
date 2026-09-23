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

// This file covers the server-side monitor. The client's handler — the
// mirror, against the misframing server in ../misframe — is tested beside the
// client binary.

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

// newMonitoredServer starts the same mux main() serves, on a real HTTP/1.1
// listener: an upgrade needs a hijackable connection.
func newMonitoredServer(tb testing.TB) (*httptest.Server, *faultMonitor) {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	monitor := newFaultMonitor()
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server,
		connectwebsocket.WithServerProtocolErrorHandler(monitor.observe),
	)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer, monitor
}

// sendOneFrame opens a connection, writes one frame, and reads the verdict.
func sendOneFrame(tb testing.TB, serverURL string, text bool, marker rune, payload []byte) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(serverURL, "http") + pingv1connect.PingServiceCumSumProcedure
	conn, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols: []string{"connect.v2+proto"},
	})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		tb.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	// Every stream opens with one M message; the frame under test follows it.
	if err := conn.Write(ctx, websocket.MessageText, []byte("M{}")); err != nil {
		tb.Fatalf("write opening metadata: %v", err)
	}

	frame := append([]byte(string(marker)), payload...)
	messageType := websocket.MessageBinary
	if text {
		messageType = websocket.MessageText
	}
	if err := conn.Write(ctx, messageType, frame); err != nil {
		tb.Fatalf("write: %v", err)
	}
	_, _, _ = conn.Read(ctx)
}

// The monitor counts repeated mistakes from one client, which is the point of
// keying on the host rather than on PeerAddr: one connection carries one RPC,
// so a per-connection counter could never exceed one.
func TestMonitorCountsRepeatOffenders(t *testing.T) {
	httpServer, monitor := newMonitoredServer(t)
	message, err := proto.Marshal(&v1.CumSumRequest{Number: 7})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Two unknown markers, on two connections.
	sendOneFrame(t, httpServer.URL, false, 'Z', message)
	sendOneFrame(t, httpServer.URL, false, 'Y', message)
	// And one of a different kind, which must count separately.
	sendOneFrame(t, httpServer.URL, true, 'B', nil)

	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if len(monitor.counts) != 1 {
		t.Fatalf("got %d clients; want 1 (all faults came from one host)", len(monitor.counts))
	}
	for client, byFault := range monitor.counts {
		if got := byFault[connectwebsocket.FaultMarker]; got != 2 {
			t.Errorf("%s: got %d marker faults; want 2", client, got)
		}
		if got := byFault[connectwebsocket.FaultFrameType]; got != 1 {
			t.Errorf("%s: got %d frame_type faults; want 1", client, got)
		}
	}
}

// A well-framed client produces nothing: the handler reports faults, not
// traffic.
func TestMonitorSilentOnCleanTraffic(t *testing.T) {
	httpServer, monitor := newMonitoredServer(t)
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	if err != nil {
		t.Fatalf("websocket transport: %v", err)
	}
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	stream, err := client.CumSum(t.Context())
	if err != nil {
		t.Fatalf("CumSum: %v", err)
	}
	if err := stream.Send(&v1.CumSumRequest{Number: 1}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if len(monitor.counts) != 0 {
		t.Errorf("got %d clients reported; want 0", len(monitor.counts))
	}
}
