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

	"connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/generics/connect/ping/v1/pingv1connect"
)

func TestHandlerClientUnary(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(handlerClientPingServer{}))

	hc := connect.NewHandlerClient(mux)
	t.Cleanup(func() { _ = hc.Close() })

	client := pingv1connect.NewPingServiceClient(hc, hc.URL())
	response, err := client.Ping(t.Context(), connect.NewRequest(&pingv1.PingRequest{Number: 42}))
	assert.Nil(t, err)
	assert.Equal(t, response.Msg.GetNumber(), 42)
}

func TestHandlerClientUnaryGRPC(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(handlerClientPingServer{}))

	hc := connect.NewHandlerClient(mux)
	t.Cleanup(func() { _ = hc.Close() })

	client := pingv1connect.NewPingServiceClient(hc, hc.URL(), connect.WithGRPC())
	response, err := client.Ping(t.Context(), connect.NewRequest(&pingv1.PingRequest{Number: 7}))
	assert.Nil(t, err)
	assert.Equal(t, response.Msg.GetNumber(), 7)
}

func TestHandlerClientBidiStream(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(handlerClientPingServer{}))

	hc := connect.NewHandlerClient(mux)
	t.Cleanup(func() { _ = hc.Close() })

	client := pingv1connect.NewPingServiceClient(hc, hc.URL(), connect.WithGRPC())
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

func TestHandlerClientCloseIdempotent(t *testing.T) {
	t.Parallel()
	hc := connect.NewHandlerClient(http.NotFoundHandler())
	assert.Nil(t, hc.Close())
	assert.Nil(t, hc.Close())
}

// handlerClientPingServer is a minimal PingService implementation used to
// exercise NewHandlerClient end-to-end without depending on the richer
// fixtures in connect_ext_test.go.
type handlerClientPingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (handlerClientPingServer) Ping(
	_ context.Context,
	request *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {
	return connect.NewResponse(&pingv1.PingResponse{
		Number: request.Msg.GetNumber(),
		Text:   request.Msg.GetText(),
	}), nil
}

func (handlerClientPingServer) CumSum(
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
