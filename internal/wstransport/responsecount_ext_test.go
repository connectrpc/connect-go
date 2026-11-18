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
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/wstransport"
	"google.golang.org/protobuf/proto"
)

// serverFrame is one unmasked, unfragmented server frame. It covers payloads
// under 126 bytes, which is all these scripts send.
func serverFrame(text bool, data []byte) []byte {
	opcode := byte(0x82)
	if text {
		opcode = 0x81
	}
	return append([]byte{opcode, byte(len(data))}, data...)
}

// scriptedResponseServer completes one handshake by hand and writes frames,
// then waits for the client to close. It returns the base URL.
func scriptedResponseServer(tb testing.TB, frames ...[]byte) string {
	tb.Helper()
	listener, err := (&net.ListenConfig{}).Listen(tb.Context(), "tcp", "127.0.0.1:0")
	assert.Nil(tb, err)
	tb.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, reader := acceptRawUpgrade(tb, listener)
		for _, frame := range frames {
			if _, err := conn.Write(frame); err != nil {
				return
			}
		}
		_, _, _ = watchFrames(reader, nil)
	}()
	return "http://" + listener.Addr().String()
}

// §6.2: a unary call returns exactly one message. A server that ends it with
// none, or sends two, has broken the RPC's cardinality, and the client reports
// unimplemented, as Connect over HTTP does.
func TestUnaryResponseCountIsEnforced(t *testing.T) {
	t.Parallel()
	body, err := proto.Marshal(&pingv1.PingResponse{Number: 1})
	assert.Nil(t, err)
	bodyFrame := serverFrame(false, append([]byte{wireBody}, body...))
	opening := serverFrame(true, []byte("M[]"))
	end := serverFrame(true, []byte("S{}"))
	for _, test := range []struct {
		name   string
		frames [][]byte
		want   string
	}{
		{name: "zero messages", frames: [][]byte{opening, end}, want: "zero messages"},
		{name: "two messages", frames: [][]byte{opening, bodyFrame, bodyFrame, end}, want: "multiple messages"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport, err := wstransport.NewTransport(scriptedResponseServer(t, test.frames...))
			assert.Nil(t, err)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, err = client.Ping(ctx, &pingv1.PingRequest{Number: 1})
			if !assert.NotNil(t, err) {
				return
			}
			assert.Equal(t, connect.CodeOf(err), connect.CodeUnimplemented)
			assert.True(t, strings.Contains(err.Error(), test.want))
		})
	}
}

// §13: a 1007 close is one of the two a client may take an outcome from. The
// server failed the connection over this client's text, so the RPC is
// Internal rather than the Unavailable of a connection that simply dropped.
func TestInvalidUTF8CloseFromServerIsInternal(t *testing.T) {
	t.Parallel()
	reason := "text message is not valid UTF-8"
	closeFrame := append([]byte{0x88, byte(2 + len(reason)), 0x03, 0xEF}, reason...)
	url := scriptedResponseServer(t, serverFrame(true, []byte("M[]")), closeFrame)
	transport, err := wstransport.NewTransport(url)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	// The server closes at once, so the write may find the connection gone.
	// As over HTTP, that reads as io.EOF and the verdict comes from Receive.
	if sendErr := stream.Send(&pingv1.CumSumRequest{Number: 1}); sendErr != nil {
		assert.True(t, errors.Is(sendErr, io.EOF))
	}
	_, err = stream.Receive()
	if !assert.NotNil(t, err) {
		return
	}
	assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
	assert.True(t, strings.Contains(err.Error(), "invalid UTF-8"))
	_ = stream.Close()
}
