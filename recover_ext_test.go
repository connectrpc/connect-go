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
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/internal/memhttp/memhttptest"
)

type panicPingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	panicWith any
}

func (s *panicPingServer) Ping(
	_ context.Context,
	_ *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {
	panic(s.panicWith) //nolint:forbidigo
}

func (s *panicPingServer) Sum(
	_ context.Context,
	stream *connect.ClientStream[pingv1.SumRequest],
) (*connect.Response[pingv1.SumResponse], error) {
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, err
		}
	}
	panic(s.panicWith) //nolint:forbidigo
}

func (s *panicPingServer) CountUp(
	_ context.Context,
	_ *connect.Request[pingv1.CountUpRequest],
	stream *connect.ServerStream[pingv1.CountUpResponse],
) error {
	if err := stream.Send(&pingv1.CountUpResponse{}); err != nil {
		return err
	}
	panic(s.panicWith) //nolint:forbidigo
}

func (s *panicPingServer) CumSum(
	_ context.Context,
	stream *connect.BidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse],
) error {
	req, err := stream.Receive()
	if err != nil {
		return err
	}
	if err := stream.Send(&pingv1.CumSumResponse{Sum: req.Number}); err != nil {
		return err
	}
	panic(s.panicWith) //nolint:forbidigo
}

func TestWithRecover(t *testing.T) {
	t.Parallel()
	handle := func(_ context.Context, _ connect.Spec, _ http.Header, r any) error {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("panic: %v", r))
	}
	assertHandled := func(err error) {
		t.Helper()
		assert.NotNil(t, err)
		assert.Equal(t, connect.CodeOf(err), connect.CodeFailedPrecondition)
	}
	assertNotHandled := func(err error) {
		t.Helper()
		// When HTTP/2 handlers panic, net/http sends an RST_STREAM frame with code
		// INTERNAL_ERROR. We should be mapping this back to CodeInternal.
		assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
	}
	drainStream := func(stream *connect.ServerStreamForClient[pingv1.CountUpResponse]) error {
		t.Helper()
		defer assertNoError(t, stream.Close)
		assert.True(t, stream.Receive())  // expect one response msg
		assert.False(t, stream.Receive()) // expect panic before second response msg
		return stream.Err()
	}
	pinger := &panicPingServer{}
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pinger, connect.WithRecover(handle)))
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL(),
	)

	for _, panicWith := range []any{42, nil} {
		pinger.panicWith = panicWith

		_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{}))
		assertHandled(err)

		stream, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{}))
		assert.Nil(t, err)
		assertHandled(drainStream(stream))
	}

	pinger.panicWith = http.ErrAbortHandler

	_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{}))
	assertNotHandled(err)

	stream, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{}))
	assert.Nil(t, err)
	assertNotHandled(drainStream(stream))
}

//nolint:tparallel // we can't run sub-tests in parallel due to server's statefulness
func TestRecoverInterceptor(t *testing.T) {
	t.Parallel()
	var check atomic.Value
	handle := func(ctx context.Context, req connect.AnyRequest, panicVal any) error {
		fn, ok := check.Load().(func(context.Context, connect.AnyRequest, any))
		if ok {
			fn(ctx, req, panicVal)
		}
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("panic: %v", panicVal))
	}
	assertHandled := func(err error) {
		t.Helper()
		assert.NotNil(t, err)
		assert.Equal(t, connect.CodeOf(err), connect.CodeFailedPrecondition)
	}
	drainServerStream := func(stream *connect.ServerStreamForClient[pingv1.CountUpResponse]) error {
		t.Helper()
		defer assertNoError(t, stream.Close)
		assert.True(t, stream.Receive())  // expect one response msg
		assert.False(t, stream.Receive()) // expect panic before second response msg
		return stream.Err()
	}
	drainBidiStream := func(stream *connect.BidiStreamForClient[pingv1.CumSumRequest, pingv1.CumSumResponse]) error {
		t.Helper()
		assertNoError(t, stream.CloseRequest)
		defer assertNoError(t, stream.CloseResponse)
		resp, err := stream.Receive()
		// expect one response msg
		assert.NotNil(t, resp)
		assert.Nil(t, err)
		// expect panic before second response msg
		resp, err = stream.Receive()
		assert.Nil(t, resp)
		return err
	}
	pinger := &panicPingServer{}
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pinger,
		connect.WithInterceptors(connect.RecoverInterceptor(handle))))
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL(),
	)

	// Unary and server-stream RPCs return the request message from req.Any()
	check.Store(func(ctx context.Context, req connect.AnyRequest, panicVal any) {
		assert.NotNil(t, req.Any())
	})

	t.Run("unary", func(t *testing.T) { //nolint:paralleltest
		for _, panicWith := range []any{42, nil, http.ErrAbortHandler} {
			pinger.panicWith = panicWith

			_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{}))
			assertHandled(err)
		}
	})

	t.Run("server-stream", func(t *testing.T) { //nolint:paralleltest
		for _, panicWith := range []any{42, nil, http.ErrAbortHandler} {
			pinger.panicWith = panicWith

			stream, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{}))
			assert.Nil(t, err)
			assertHandled(drainServerStream(stream))
		}
	})

	// But client-stream and bidi-stream RPCs return nil from req.Any()
	check.Store(func(ctx context.Context, req connect.AnyRequest, panicVal any) {
		assert.Nil(t, req.Any())
	})

	t.Run("client-stream", func(t *testing.T) { //nolint:paralleltest
		for _, panicWith := range []any{42, nil, http.ErrAbortHandler} {
			pinger.panicWith = panicWith

			stream := client.Sum(context.Background())
			err := stream.Send(&pingv1.SumRequest{Number: 123})
			assert.Nil(t, err)
			resp, err := stream.CloseAndReceive()
			assert.Nil(t, resp)
			assertHandled(err)
		}
	})

	t.Run("bidi-stream", func(t *testing.T) { //nolint:paralleltest
		for _, panicWith := range []any{42, nil, http.ErrAbortHandler} {
			pinger.panicWith = panicWith

			stream := client.CumSum(context.Background())
			err := stream.Send(&pingv1.CumSumRequest{Number: 123})
			assert.Nil(t, err)
			assertHandled(drainBidiStream(stream))
		}
	})
}

func assertNoError(t *testing.T, fn func() error) {
	t.Helper()
	err := fn()
	assert.Nil(t, err)
}
