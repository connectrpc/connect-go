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
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

// The shape of a client-streaming RPC on the wire: an M message, several
// values, a C message; then the server's own M, one response message, one S,
// and the server's close. Written as a reference trace, because a second
// implementation has to reproduce it exactly.
func TestClientStreamingWireSequence(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, scriptedServer{})
	client := dialBrowserClient(t, httpServer, pingv1connect.PingServiceSumProcedure)

	var want int64
	for _, value := range []int64{1, 2, 3, 4} {
		payload, err := proto.Marshal(&pingv1.SumRequest{Number: value})
		assert.Nil(t, err)
		client.writeProto(t, payload)
		want += value
	}
	// §6.1: C is bare and always text, whatever the codec.
	client.writeJSON(t, wireClientEndStream, nil)

	// The response is three messages: the server's own opening M, the body,
	// then the end-of-stream message carrying the trailers. A client that
	// stops after the body silently drops them.
	marker, payload, text := client.readMessage(t)
	assert.Equal(t, marker, wireMetadata)
	assert.True(t, text)                   // metadata is JSON
	assert.Equal(t, string(payload), "[]") // this handler sets none

	marker, payload, text = client.readMessage(t)
	assert.Equal(t, marker, wireBody)
	assert.False(t, text) // Protobuf body, binary frame
	var response pingv1.SumResponse
	assert.Nil(t, proto.Unmarshal(payload, &response))
	assert.Equal(t, response.Sum, want)

	marker, payload, text = client.readMessage(t)
	assert.Equal(t, marker, wireServerEndStream)
	assert.True(t, text) // EndStreamMessage JSON, text frame
	assert.True(t, strings.Contains(string(payload), "set-by-handler"))
	assert.True(t, !strings.Contains(string(payload), "error"))

	// Nothing follows but the close, which the server sends itself: the RPC is
	// over, and one connection carries only the one RPC.
	readCtx, cancelRead := readContext(t)
	defer cancelRead()
	_, _, err := client.conn.Read(readCtx)
	assert.NotNil(t, err)
	assert.Equal(t, websocket.CloseStatus(err), websocket.StatusNormalClosure)
}

// §6.1: C carries no payload and is always text, so an empty final message
// can never be mistaken for it. A C with a payload, or one in a binary frame,
// is a protocol error rather than a final message to deliver.
func TestEndOfClientStreamMustBeBareText(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		text bool
		want string
	}{
		{name: "payload in a binary frame", want: "C in a binary frame; expected a text frame"},
		{name: "payload in a text frame", text: true, want: "C carries none"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer(t, pingServer{})
			conn := dialCumSum(t, httpServer, "")
			payload, err := proto.Marshal(&pingv1.CumSumRequest{Number: 1})
			assert.Nil(t, err)
			if test.text {
				payload = []byte(`{"number":"1"}`)
			}
			sendWireMessage(t, conn, test.text, wireClientEndStream, payload)

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			assert.True(t, strings.Contains(string(data), "internal"))
			assert.True(t, strings.Contains(string(data), test.want))
		})
	}
}

// The same exchange through the generated client, which is what a Go caller
// actually writes. CloseAndReceive sends End-Of-Client-Stream, reads the single
// response, and reads the end-of-stream message behind it so the trailers survive.
func TestClientStreamingThroughTheGeneratedClient(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, scriptedServer{})
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectStreaming),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	ctx, info := connect.NewClientContext(t.Context())
	stream, err := client.Sum(ctx)
	assert.Nil(t, err)
	var want int64
	for _, value := range []int64{1, 2, 3, 4} {
		assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: value}))
		want += value
	}
	response, err := stream.CloseAndReceive()
	assert.Nil(t, err)
	assert.Equal(t, response.Sum, want)
	assert.Equal(t, info.ResponseTrailer().Get(trailerKey), "set-by-handler")
}

// A client that keeps sending after its C message must not stall. The server
// has stopped delivering those messages, but it goes on reading and discarding
// them, so the peer's writes never block on a full socket buffer.
//
// The handler lingers on purpose, holding the connection open: once it
// returns, the connection closes and the writes drain into nothing, which
// would hide a stall. The assertion is that the writes finish while the
// handler is still in that window — without the drain they instead wait it
// out, because a full buffer only clears when the connection dies.
func TestMessagesAfterEndOfClientStreamAreDiscarded(t *testing.T) {
	t.Parallel()
	const linger = 5 * time.Second
	handlerDone := make(chan struct{})
	httpServer := newHybridServer2(t, lingeringServer{linger: linger, done: handlerDone})
	conn := dialCumSum(t, httpServer, "")
	conn.SetReadLimit(-1)
	sendProtoBody(t, conn, &pingv1.CumSumRequest{Number: 1})
	sendJSONMessage(t, conn, wireClientEndStream, nil)

	// More than any socket buffer will hold.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	body := make([]byte, 64*1024)
	for range 256 {
		if err := conn.Write(ctx, websocket.MessageBinary, append([]byte("B"), body...)); err != nil {
			t.Fatalf("write blocked or failed after C: %v", err)
		}
	}

	select {
	case <-handlerDone:
		t.Fatal("writes after C outlasted the handler, so nothing was draining them")
	default:
	}
}

// lingeringServer reads to EOF and then stays in the handler, holding the
// connection open, before signalling that it has left.
type lingeringServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	linger time.Duration
	done   chan struct{}
}

func (s lingeringServer) CumSum(
	_ context.Context,
	stream pingv1connect.PingServiceCumSumServerStream,
) error {
	for {
		if _, err := stream.Receive(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}
	time.Sleep(s.linger)
	close(s.done)
	return nil
}
