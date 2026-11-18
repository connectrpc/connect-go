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

package wstransport

import (
	"context"
	"net"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// ResponseHeader dials a stream that has not dialed yet. The metadata lock it
// shares with Receive and ResponseTrailer must not be held for the handshake.
func TestResponseHeaderDoesNotLockDuringHandshake(t *testing.T) {
	t.Parallel()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	assert.Nil(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		// Accepts the connection and never answers the upgrade.
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()

	transport, err := NewTransport("http://" + listener.Addr().String())
	assert.Nil(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := transport.NewClientStream(ctx, connect.Spec{
		Procedure:  pingv1connect.PingServiceCumSumProcedure,
		StreamType: connect.StreamTypeBidi,
	})
	assert.Nil(t, err)
	wsStream, ok := stream.(*clientStream)
	assert.True(t, ok)
	conn := wsStream.conn
	headerDone := make(chan struct{})
	go func() {
		defer close(headerDone)
		_ = conn.ResponseHeader()
	}()
	select {
	case serverConn := <-accepted:
		t.Cleanup(func() { _ = serverConn.Close() })
	case <-time.After(5 * time.Second):
		t.Fatal("client never dialed")
	}

	trailerDone := make(chan struct{})
	go func() {
		defer close(trailerDone)
		_ = conn.ResponseTrailer()
	}()
	select {
	case <-trailerDone:
	case <-time.After(time.Second):
		t.Error("ResponseTrailer waited for the handshake")
	}
	cancel()
	<-headerDone
	<-trailerDone
}
