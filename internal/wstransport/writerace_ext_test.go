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
	"crypto/rand"
	"encoding/base64"
	"net"
	"testing"
	"time"

	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/wstransport"
	"github.com/coder/websocket"
)

// deadlineMillis is the deadline the race tests send, and resumeAfter when
// their clients start reading again: after it, while a write is in flight.
const (
	deadlineMillis = "300"
	resumeAfter    = 350 * time.Millisecond
)

// bigPingServer answers with more than the socket buffers hold, so its write
// is still in flight while a client that is not reading holds it up.
type bigPingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	text string
}

func (s bigPingServer) Ping(context.Context, *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	return &pingv1.PingResponse{Text: s.text}, nil
}

// §11.1: an expired deadline still ends in S and 1000. The library closes the
// connection when a write's context ends, so a write under the RPC's context
// lost both whenever the deadline caught it mid-write.
func TestWriteInFlightAtDeadlineStillEndsWithS(t *testing.T) {
	t.Parallel()
	// Random, so that permessage-deflate cannot shrink it below the buffers.
	random := make([]byte, 24<<20)
	_, _ = rand.Read(random)
	httpServer := newHybridServer2(t, bigPingServer{text: base64.StdEncoding.EncodeToString(random)})
	conn := dialRaw(t, httpServer, pingv1connect.PingServicePingProcedure+"?connect-timeout-ms="+deadlineMillis)
	conn.SetReadLimit(-1)
	sendJSONMessage(t, conn, wireMetadata, []byte("[]"))
	sendProtoBody(t, conn, &pingv1.PingRequest{})
	sendJSONMessage(t, conn, wireClientEndStream, nil)

	time.Sleep(resumeAfter)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var last byte
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			assert.Equal(t, websocket.CloseStatus(err), websocket.StatusNormalClosure)
			break
		}
		if len(data) > 0 {
			last = data[0]
		}
	}
	assert.Equal(t, last, wireServerEndStream)
}

// The same holds for a keep-alive Ping awaiting its Pong when the deadline
// expires: the library closes the connection when the Ping's context ends.
func TestPingInFlightAtDeadlineStillEndsWithS(t *testing.T) {
	t.Parallel()
	httpServer := newKeepAliveServer(t,
		wstransport.WithKeepAlive(keepAliveInterval),
		wstransport.WithKeepAliveTimeout(time.Minute),
	)
	addr := httpServer.Listener.Addr().String()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_ = rawHandshake(t, conn, addr, pingProcedure+"?connect-timeout-ms="+deadlineMillis)
	assert.Nil(t, writeClientFrame(conn, openMessage(), false, true))

	// Never answering, so a Ping is always awaiting its Pong.
	assert.Nil(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	pings, closeCode, _ := watchFrames(conn, nil)
	assert.True(t, pings >= 1)
	assert.Equal(t, closeCode, 1000)
}
