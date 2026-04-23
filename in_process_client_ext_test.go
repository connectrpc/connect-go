// Copyright 2021-2025 The Connect Authors
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

package connect_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/generics/connect/ping/v1/pingv1connect"
)

func TestInProcessClientUnary(t *testing.T) {
	t.Parallel()
	server := &pluggablePingServer{ping: echoPing}
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(server))

	client := pingv1connect.NewPingServiceClient(connect.NewInProcessHTTPClient(mux))
	response, err := client.Ping(t.Context(), connect.NewRequest(&pingv1.PingRequest{Number: 42}))
	assert.Nil(t, err)
	assert.Equal(t, response.Msg.GetNumber(), 42)
}

func TestInProcessClientUnaryGRPC(t *testing.T) {
	t.Parallel()
	server := &pluggablePingServer{ping: echoPing}
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(server))

	hc, url := connect.NewInProcessHTTPClient(mux)
	client := pingv1connect.NewPingServiceClient(hc, url, connect.WithGRPC())
	response, err := client.Ping(t.Context(), connect.NewRequest(&pingv1.PingRequest{Number: 7}))
	assert.Nil(t, err)
	assert.Equal(t, response.Msg.GetNumber(), 7)
}

func TestInProcessClientBidiStream(t *testing.T) {
	t.Parallel()
	server := &pluggablePingServer{cumSum: cumSumStream}
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(server))

	hc, url := connect.NewInProcessHTTPClient(mux)
	client := pingv1connect.NewPingServiceClient(hc, url, connect.WithGRPC())
	stream := client.CumSum(t.Context())
	inputs := []int64{1, 2, 3, 4}
	want := []int64{1, 3, 6, 10}

	for _, n := range inputs {
		assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: n}))
	}
	assert.Nil(t, stream.CloseRequest())

	var got []int64
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		assert.Nil(t, err)
		got = append(got, msg.GetSum())
	}
	assert.Nil(t, stream.CloseResponse())
	assert.Equal(t, got, want)
}

// TestInProcessClientCancelPropagation verifies that cancelling the
// caller's context unwinds the handler's request context, even though
// NewInProcessHTTPClient uses a separate server-side base context. HTTP/2
// RST_STREAM carries the cancellation across the in-memory pipe.
func TestInProcessClientCancelPropagation(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	done := make(chan error, 1)

	server := &pluggablePingServer{
		ping: func(ctx context.Context, _ *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
			close(started)
			<-ctx.Done()
			done <- ctx.Err()
			return nil, ctx.Err()
		},
	}
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(server))

	client := pingv1connect.NewPingServiceClient(connect.NewInProcessHTTPClient(mux))

	ctx, cancel := context.WithCancel(t.Context())
	clientErr := make(chan error, 1)
	go func() {
		_, err := client.Ping(ctx, connect.NewRequest(&pingv1.PingRequest{Number: 1}))
		clientErr <- err
	}()

	<-started
	cancel()

	select {
	case err := <-done:
		assert.True(t, errors.Is(err, context.Canceled))
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not observe cancellation within 2s")
	}
	<-clientErr
}

func echoPing(
	_ context.Context,
	request *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {
	return connect.NewResponse(&pingv1.PingResponse{
		Number: request.Msg.GetNumber(),
		Text:   request.Msg.GetText(),
	}), nil
}

func cumSumStream(
	_ context.Context,
	stream *connect.BidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse],
) error {
	var sum int64
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		sum += msg.GetNumber()
		if err := stream.Send(&pingv1.CumSumResponse{Sum: sum}); err != nil {
			return err
		}
	}
}
