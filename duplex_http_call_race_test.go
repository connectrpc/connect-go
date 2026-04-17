// Copyright 2021-2025 The Connect Authors
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
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/generics/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/internal/memhttp/memhttptest"
)

// TestDuplexHTTPCallSendCloseAndReceiveNoNilDeref is a regression test for a
// nil pointer dereference in duplexHTTPCall when a client-streaming RPC's
// Send and CloseAndReceive are invoked concurrently.
//
// The failure mode: duplexHTTPCall.Send initialises requestBodyWriter only in
// the branch where it wins the CompareAndSwap of requestSent. CloseWrite
// (reached via CloseAndReceive → CloseRequest) can win the same CAS first and
// return without ever initialising requestBodyWriter. A subsequent Send then
// observes isFirst=false, skips the pipe setup, and runs
// payload.WriteTo(d.requestBodyWriter) with requestBodyWriter==nil, crashing
// with "invalid memory address or nil pointer dereference" at
// io.(*PipeWriter).Write.
//
// Concurrent Send and CloseWrite are explicitly expected to be safe (see the
// "This runs concurrently with Write and CloseWrite" comment on
// duplexHTTPCall.makeRequest), so the nil-deref is a bug in duplexHTTPCall.
//
// This test provokes the bad ordering deterministically by delaying the
// sender goroutine's first Send until after the main goroutine has entered
// CloseAndReceive. With the bug present, the sender goroutine panics and
// tears down the test binary; with the fix, the race is no longer reachable
// and the test completes cleanly (also under -race).
func TestDuplexHTTPCallSendCloseAndReceiveNoNilDeref(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(&pluggablePingServer{
		sum: func(_ context.Context, stream *connect.ClientStream[pingv1.SumRequest]) (*connect.Response[pingv1.SumResponse], error) {
			// Drain the stream; we only care about cleanly returning so the
			// client's CloseAndReceive unblocks.
			for stream.Receive() {
			}
			return connect.NewResponse(&pingv1.SumResponse{}), nil
		},
	}))
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())

	// A handful of iterations is enough: the bad ordering is forced
	// deterministically below.
	const iterations = 5
	for range iterations {
		stream := client.Sum(t.Context())

		// Start a goroutine that issues a Send after a short delay. The
		// delay lets the main goroutine's CloseAndReceive reach CloseWrite
		// first, winning the CAS on requestSent. The late Send then
		// observes isFirst=false and — in the buggy implementation — reads
		// requestBodyWriter==nil and nil-derefs.
		sendDone := make(chan struct{})
		go func() {
			defer close(sendDone)
			time.Sleep(50 * time.Millisecond)
			// Ignore the error: with the bug present this panics before
			// returning; with the fix it returns nil or io.EOF depending
			// on whether CloseAndReceive has already completed.
			_ = stream.Send(&pingv1.SumRequest{Number: 1})
		}()

		if _, err := stream.CloseAndReceive(); err != nil {
			t.Fatalf("CloseAndReceive: %v", err)
		}

		// Wait for the sender goroutine to finish. With the bug present it
		// will have panicked already; with the fix it returns cleanly.
		<-sendDone
	}
}
