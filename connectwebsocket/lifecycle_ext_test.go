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

package connectwebsocket_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// openBlockedStream leaves a stream where the next Receive blocks: the client
// has had its reply and the server is waiting for more input that never comes.
// That is the state a mid-stream cancellation has to interrupt.
func openBlockedStream(ctx context.Context, tb testing.TB) interface {
	Receive() (*pingv1.CumSumResponse, error)
	Close() error
} {
	tb.Helper()
	client := newBidiClient(tb)
	stream, err := client.CumSum(ctx)
	assert.Nil(tb, err)
	assert.Nil(tb, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	_, err = stream.Receive()
	assert.Nil(tb, err)
	return stream
}

// Cancelling mid-stream must surface as Canceled, not as a transport failure a
// caller might retry.
func TestCancelDuringBlockedReceive(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	stream := openBlockedStream(ctx, t)
	t.Cleanup(func() { _ = stream.Close() })

	received := make(chan error, 1)
	go func() {
		_, err := stream.Receive()
		received <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the read block
	cancel()

	select {
	case err := <-received:
		assert.NotNil(t, err)
		assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
	case <-time.After(10 * time.Second):
		t.Fatal("Receive did not unblock when the context was canceled")
	}
}

// The same for a deadline, which must not be reported as cancellation.
func TestDeadlineDuringBlockedReceive(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	stream := openBlockedStream(ctx, t)
	t.Cleanup(func() { _ = stream.Close() })

	received := make(chan error, 1)
	go func() {
		_, err := stream.Receive()
		received <- err
	}()

	select {
	case err := <-received:
		assert.NotNil(t, err)
		assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded)
	case <-time.After(10 * time.Second):
		t.Fatal("Receive did not unblock when the deadline passed")
	}
}

// panicServer models the handler nobody means to write.
type panicServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (panicServer) CumSum(_ context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	if _, err := stream.Receive(); err != nil {
		return err
	}
	panic("handler exploded")
}

// A panicking handler must not strand the caller. net/http recovers the panic
// but deliberately leaves a hijacked connection open — "if !c.hijacked()" — so
// unlike an HTTP handler, nothing closes the socket on the way out.
func TestHandlerPanicDoesNotStrandTheClient(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer2(t, panicServer{})
	// The panic trace net/http prints is expected here, so discard it.
	httpServer.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	t.Cleanup(func() { _ = stream.Close() })
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))

	received := make(chan error, 1)
	go func() {
		_, err := stream.Receive()
		received <- err
	}()
	select {
	case err := <-received:
		t.Logf("client saw: %v", err)
		assert.NotNil(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("client hung: the panicking handler left the connection open")
	}
}

// Once the client has said it is done sending, a further Send has nowhere to
// go: the peer has stopped reading data envelopes. Reporting it keeps the
// caller from believing the message was delivered, and matches connecthttp.
func TestSendAfterCloseSendFails(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	t.Cleanup(func() { _ = stream.Close() })
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	_, err = stream.Receive()
	assert.Nil(t, err)
	assert.Nil(t, stream.CloseSend())

	assert.NotNil(t, stream.Send(&pingv1.CumSumRequest{Number: 2}))
	// CloseSend stays idempotent: only Send is affected.
	assert.Nil(t, stream.CloseSend())
}
