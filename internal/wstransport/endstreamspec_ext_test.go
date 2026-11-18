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
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/wstransport"
	"github.com/coder/websocket"
)

// readCloseStatus reads past whatever the server sends until its Close frame.
func readCloseStatus(tb testing.TB, conn *websocket.Conn) websocket.StatusCode {
	tb.Helper()
	ctx, cancel := readContext(tb)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

// §8 and §12.1: an oversized message after C is still a message after C. It
// cannot be read, so the server closes with 1009 after its S.
func TestOversizedMessageAfterEndOfClientStreamIsRejected(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, lingeringServer{
		linger: 300 * time.Millisecond,
		done:   make(chan struct{}),
	}, wstransport.WithReadMaxBytes(1024))
	conn := dialCumSum(t, httpServer, "")
	sendProtoBody(t, conn, &pingv1.CumSumRequest{Number: 1})
	sendJSONMessage(t, conn, wireClientEndStream, nil)
	ctx, cancel := readContext(t)
	defer cancel()
	assert.Nil(t, conn.Write(ctx, websocket.MessageBinary, append([]byte{wireBody}, make([]byte, 4096)...)))

	data := readServerOpening(t, conn)
	assert.Equal(t, data[0], wireServerEndStream)
	assert.True(t, strings.Contains(string(data), "resource_exhausted"), assert.Sprintf("%s", data))
	assert.Equal(t, readCloseStatus(t, conn), websocket.StatusMessageTooBig)
}

// §9: the S payload is an EndStreamResponse, a JSON object. null is not one,
// and must not read as success.
func TestNullEndStreamIsAProtocolError(t *testing.T) {
	t.Parallel()
	url := scriptedResponseServer(t, serverFrame(true, []byte("M[]")), serverFrame(true, []byte("Snull")))
	transport, err := wstransport.NewTransport(url)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 1})
	assert.Nil(t, err)
	_, err = stream.Receive()
	assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
	assert.True(t, err != nil && strings.Contains(err.Error(), "JSON EndStreamMessage object"))
	// The scripted server never answers the client's Close frame, so the
	// closing handshake waits out its timeout.
	go func() { _ = stream.Close() }()
}

// errorServer fails every RPC with a long message.
type errorServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (errorServer) Ping(context.Context, *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	return nil, connect.NewError(connect.CodeFailedPrecondition, strings.Repeat("x", 200))
}

// §9: a server ends every stream with exactly one S. The send limit bounds RPC
// messages, so it cannot suppress the verdict.
func TestSendLimitDoesNotSuppressTheVerdict(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, errorServer{}, wstransport.WithSendMaxBytes(16))
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
	assert.Equal(t, connect.CodeOf(err), connect.CodeFailedPrecondition)
}
