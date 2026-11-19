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
	"net"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/wstransport"
)

// faultingServer answers count handshakes on listener with M and then a
// message with an unknown marker, and completes each closing handshake.
func faultingServer(tb testing.TB, listener net.Listener, count int) {
	tb.Helper()
	opening := serverFrame(true, []byte("M[]"))
	unknownMarker := serverFrame(true, []byte("X{}"))
	closeFrame := []byte{0x88, 0x02, 0x03, 0xe8}
	var group sync.WaitGroup
	defer group.Wait()
	for range count {
		conn, reader := acceptRawUpgrade(tb, listener)
		group.Go(func() {
			if _, err := conn.Write(append(opening, unknownMarker...)); err != nil {
				return
			}
			_, _, _ = watchFrames(reader, nil)
			_, _ = conn.Write(closeFrame)
		})
	}
}

// The ClientStream contract lets Close run concurrently with Receive, so the
// fault a Receive records must be safe for Close to read. Run with -race.
func TestCloseDuringFaultingReceive(t *testing.T) {
	t.Parallel()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	assert.Nil(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	const attempts = 20
	served := make(chan struct{})
	go func() {
		defer close(served)
		faultingServer(t, listener, attempts)
	}()
	transport, err := wstransport.NewTransport("http://" + listener.Addr().String())
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	for range attempts {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 1})
		assert.Nil(t, err)
		// Let the faulting message reach the read-ahead before both start.
		time.Sleep(10 * time.Millisecond)
		start := make(chan struct{})
		var group sync.WaitGroup
		group.Go(func() {
			<-start
			_, _ = stream.Receive()
		})
		group.Go(func() {
			<-start
			_ = stream.Close()
		})
		close(start)
		group.Wait()
		cancel()
	}
	<-served
}
