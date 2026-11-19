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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/wstransport"
)

// lifecycleWait bounds how long a test waits for the server to notice that a
// client left. Well under the 5s closing-handshake timeout, so a stall fails.
const lifecycleWait = 3 * time.Second

// idleAfterOneServer receives one message, then stops reading and waits on its
// context, which is where a handler that computes or sends spends its time.
// Once the context ends it reads whatever the server had already read for it,
// and reports how the context and the request stream each ended.
type idleAfterOneServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	received chan struct{}
	ended    chan idleOutcome
}

type idleOutcome struct {
	contextErr error
	receiveErr error
}

func (s *idleAfterOneServer) CumSum(
	ctx context.Context,
	stream pingv1connect.PingServiceCumSumServerStream,
) error {
	if _, err := stream.Receive(); err != nil {
		s.ended <- idleOutcome{receiveErr: err}
		return err
	}
	close(s.received)
	select {
	case <-ctx.Done():
	case <-time.After(wsTestReadTimeout):
		s.ended <- idleOutcome{}
		return nil
	}
	outcome := idleOutcome{contextErr: ctx.Err()}
	for {
		if _, err := stream.Receive(); err != nil {
			outcome.receiveErr = err
			break
		}
	}
	s.ended <- outcome
	return outcome.receiveErr
}

func newIdleAfterOneClient(tb testing.TB) (pingv1connect.PingServiceClient, *idleAfterOneServer) {
	tb.Helper()
	handler := &idleAfterOneServer{
		received: make(chan struct{}),
		ended:    make(chan idleOutcome, 1),
	}
	httpServer := newServerFor(tb, handler)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	return pingv1connect.NewPingServiceClient(connect.NewClient(transport)), handler
}

func awaitHandlerEnd(tb testing.TB, handler *idleAfterOneServer) idleOutcome {
	tb.Helper()
	select {
	case outcome := <-handler.ended:
		assert.True(tb, errors.Is(outcome.contextErr, context.Canceled))
		return outcome
	case <-time.After(lifecycleWait):
		tb.Fatal("handler context was not canceled after the client left")
		return idleOutcome{}
	}
}

// §8: a server whose connection ends before it sends S treats the RPC as
// canceled, whether or not it received C. The handler is not reading, so the
// server has to keep reading on its behalf to see the end at all, and the
// client's closing handshake must not wait out its timeout. The messages read
// on the handler's behalf are still delivered, and C still ends the stream.
func TestConnectionEndCancelsHandlerThatIsNotReading(t *testing.T) {
	t.Parallel()
	client, handler := newIdleAfterOneClient(t)
	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 2}))
	assert.Nil(t, stream.CloseSend())
	<-handler.received

	start := time.Now()
	_ = stream.Close()
	assert.True(t, time.Since(start) < lifecycleWait, assert.Sprintf("Close took %v", time.Since(start)))
	outcome := awaitHandlerEnd(t, handler)
	assert.True(t, errors.Is(outcome.receiveErr, io.EOF))
}

// The same when the client's context is canceled mid-stream, without C.
func TestClientCancelCancelsHandlerThatIsNotReading(t *testing.T) {
	t.Parallel()
	client, handler := newIdleAfterOneClient(t)
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	<-handler.received
	cancel()
	outcome := awaitHandlerEnd(t, handler)
	assert.Equal(t, connect.CodeOf(outcome.receiveErr), connect.CodeCanceled)
	_ = stream.Close()
}

// §8: a client that closes before S without sending C cancels the RPC. Close
// must not send C first, or the handler reads a clean io.EOF and may commit a
// stream the caller abandoned.
func TestCloseBeforeEndOfStreamCancelsInsteadOfFinishing(t *testing.T) {
	t.Parallel()
	client, handler := newIdleAfterOneClient(t)
	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	<-handler.received
	_ = stream.Close()
	outcome := awaitHandlerEnd(t, handler)
	assert.False(t, errors.Is(outcome.receiveErr, io.EOF))
	assert.Equal(t, connect.CodeOf(outcome.receiveErr), connect.CodeCanceled)
}

// Closing a stream that was never used costs nothing: no dial, and no handler
// run for an RPC nobody asked for. connecthttp behaves the same way.
func TestClosingUnusedStreamDoesNotDial(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, countUpServer{})
	mux := http.NewServeMux()
	mountBoth(mux, server)
	var upgrades atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if wstransport.IsUpgrade(request) {
			upgrades.Add(1)
		}
		mux.ServeHTTP(writer, request)
	}))
	t.Cleanup(httpServer.Close)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Close())
	assert.Equal(t, upgrades.Load(), int32(0))
}

// Close may run concurrently with Receive, and both touch the response
// metadata. Run under -race, which is how make shorttest runs it.
func TestCloseConcurrentWithReceive(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, countUpServer{})
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	for attempt := range 20 {
		stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 100})
		assert.Nil(t, err)
		received := make(chan struct{})
		go func() {
			defer close(received)
			for {
				if _, receiveErr := stream.Receive(); receiveErr != nil {
					return
				}
			}
		}()
		time.Sleep(time.Duration(attempt%3) * time.Millisecond)
		_ = stream.Close()
		<-received
	}
}
