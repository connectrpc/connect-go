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
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// newHybridServer2 mounts an arbitrary handler; newHybridServer is fixed to
// pingServer.
func newHybridServer2(tb testing.TB, handler pingv1connect.PingServiceHandler) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, handler)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

func newBidiClient(tb testing.TB) pingv1connect.PingServiceClient {
	tb.Helper()
	httpServer := newHybridServer(tb, pingServer{})
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectStreaming),
	)
	return pingv1connect.NewPingServiceClient(connect.NewClient(transport))
}

// Close is documented as safe to call while a Receive is blocked — "typically
// as defer stream.Close()" — so the two share state by design.
func TestCloseDuringBlockedReceive(t *testing.T) {
	t.Parallel()
	client := newBidiClient(t)
	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	_, err = stream.Receive()
	assert.Nil(t, err)

	// Nothing more is coming, so this Receive blocks until Close tears the
	// stream down.
	received := make(chan error, 1)
	go func() {
		_, err = stream.Receive()
		received <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the read block
	assert.Nil(t, stream.Close())

	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("Receive did not unblock after Close")
	}
}

// concurrentServer drives its side of the stream from two goroutines, which
// ServerStream permits and no other test here exercises.
type concurrentServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (concurrentServer) CumSum(_ context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	numbers := make(chan int64, 64)
	var receiving sync.WaitGroup
	receiving.Add(1)
	var receiveErr error
	go func() {
		defer receiving.Done()
		defer close(numbers)
		for {
			req, err := stream.Receive()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				receiveErr = err
				return
			}
			numbers <- req.Number
		}
	}()

	var total int64
	for number := range numbers {
		total += number
		if err := stream.Send(&pingv1.CumSumResponse{Sum: total}); err != nil {
			receiving.Wait()
			return err
		}
	}
	receiving.Wait()
	return receiveErr
}

func TestBidiConcurrentOnBothSides(t *testing.T) {
	t.Parallel()
	const messages = 50
	httpServer := newHybridServer2(t, concurrentServer{})
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectStreaming),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	sendErr := make(chan error, 1)
	go func() {
		for range messages {
			if err := stream.Send(&pingv1.CumSumRequest{Number: 1}); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- stream.CloseSend()
	}()

	var count int
	for {
		res, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		assert.Nil(t, err)
		count++
		assert.Equal(t, res.Sum, int64(count))
	}
	assert.Nil(t, <-sendErr)
	assert.Nil(t, stream.Close())
	assert.Equal(t, count, messages)
}
