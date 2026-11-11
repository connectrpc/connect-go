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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
)

// A server may end the RPC while the client is still writing. Whichever write
// discovers that must follow the ClientStream contract: Send reports io.EOF,
// CloseSend succeeds, and Receive reports the server's verdict.
func TestClientWriteAfterServerEndedReportsEOF(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		// failFirstWrite closes the connection before the client's opening M,
		// as when a server rejects the request as soon as it is upgraded.
		failFirstWrite bool
	}{
		{name: "after the client has read the verdict"},
		{name: "before the client's first write", failFirstWrite: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := endStreamServer(t, nil, "M{}", verdict)
			wire := &closingRoundTripper{base: httpServer.Client().Transport}
			wire.closed.Store(test.failFirstWrite)
			transport := connecthttp.NewTransport(
				&http.Client{Transport: wire},
				httpServer.URL,
				connecthttp.WithWebSocket(connecthttp.SelectStreaming),
			)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
			stream, err := client.CumSum(t.Context())
			assert.Nil(t, err)
			t.Cleanup(func() { _ = stream.Close() })

			if !test.failFirstWrite {
				assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
				_, err = stream.Receive()
				assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
				wire.closed.Store(true)
			}
			assert.ErrorIs(t, stream.Send(&pingv1.CumSumRequest{Number: 2}), io.EOF)
			assert.Nil(t, stream.CloseSend())
			_, err = stream.Receive()
			assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
		})
	}
}

// As over HTTP, CloseSend after the call's context has ended closes the request
// side without error, and Receive reports why the call ended.
func TestClientCloseSendAfterContextEnded(t *testing.T) {
	t.Parallel()
	httpServer := endStreamServer(t, nil, "M{}")
	wire := &closingRoundTripper{base: httpServer.Client().Transport}
	transport := connecthttp.NewTransport(
		&http.Client{Transport: wire},
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectStreaming),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = stream.Close() })

	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	// The ended context tears the connection down, so the C write fails.
	cancel()
	wire.closed.Store(true)
	assert.Nil(t, stream.CloseSend())
	_, err = stream.Receive()
	assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
}

// Once Receive has returned the server's verdict, the client writes nothing
// more: §8 has it send no C, and a B would only be discarded. Writing either
// would race the client's own reply to the server's Close frame.
func TestClientWritesNothingAfterReceivingEndStream(t *testing.T) {
	t.Parallel()
	received := make(chan []byte, 1)
	httpServer := endStreamServer(t, received, "M{}", verdict)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectStreaming),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)

	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	_, err = stream.Receive()
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
	assert.ErrorIs(t, stream.Send(&pingv1.CumSumRequest{Number: 2}), io.EOF)
	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())

	assert.Equal(t, string(<-received), string([]byte{wireMetadata, wireBody}))
}

const verdict = `S{"error":{"code":"resource_exhausted","message":"no more"}}`

// endStreamServer accepts an upgrade, writes frames as text messages, and sends
// no Close frame, so nothing races the client's writes. It reads until the
// client closes or the test ends, then sends the markers it read to received.
func endStreamServer(tb testing.TB, received chan<- []byte, frames ...string) *httptest.Server {
	tb.Helper()
	// Closed before any cleanup runs, so the read below ends even while the
	// client's Close waits for the server's Close frame.
	testDone := tb.Context().Done()
	httpServer := httptest.NewServer(http.HandlerFunc(
		func(responseWriter http.ResponseWriter, request *http.Request) {
			conn, err := websocket.Accept(responseWriter, request, &websocket.AcceptOptions{
				Subprotocols:       []string{"connectrpc.1+proto"},
				InsecureSkipVerify: true,
			})
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			for _, frame := range frames {
				_ = conn.Write(request.Context(), websocket.MessageText, []byte(frame))
			}
			// A client whose writes fail cannot send its Close frame, so the
			// test's end is what stops this read.
			readCtx, cancel := context.WithCancel(request.Context())
			defer cancel()
			go func() {
				select {
				case <-testDone:
					cancel()
				case <-readCtx.Done():
				}
			}()
			var markers []byte
			for {
				_, frame, err := conn.Read(readCtx)
				if err != nil {
					break
				}
				if len(frame) > 0 {
					markers = append(markers, frame[0])
				}
			}
			if received != nil {
				received <- markers
			}
		},
	))
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// closingRoundTripper makes writes on the upgraded connection fail once closed
// is set, as they do after the client has answered the server's Close frame.
type closingRoundTripper struct {
	base   http.RoundTripper
	closed atomic.Bool
}

func (r *closingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if body, ok := response.Body.(io.ReadWriteCloser); ok {
		response.Body = &closingConn{ReadWriteCloser: body, closed: &r.closed}
	}
	return response, nil
}

type closingConn struct {
	io.ReadWriteCloser

	closed *atomic.Bool
}

func (c *closingConn) Write(data []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	return c.ReadWriteCloser.Write(data)
}
