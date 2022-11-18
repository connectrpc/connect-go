// Copyright 2021-2022 Buf Technologies, Inc.
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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bufbuild/connect-go"
	"github.com/bufbuild/connect-go/internal/assert"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
	"github.com/bufbuild/connect-go/internal/gen/connect/ping/v1/pingv1connect"
)

func TestNewClient_InitFailure(t *testing.T) {
	t.Parallel()
	client := pingv1connect.NewPingServiceClient(
		http.DefaultClient,
		"http://127.0.0.1:8080",
		// This triggers an error during initialization, so each call will short circuit returning an error.
		connect.WithSendCompression("invalid"),
	)
	validateExpectedError := func(t *testing.T, err error) {
		t.Helper()
		assert.NotNil(t, err)
		var connectErr *connect.Error
		assert.True(t, errors.As(err, &connectErr))
		assert.Equal(t, connectErr.Message(), `unknown compression "invalid"`)
	}

	t.Run("unary", func(t *testing.T) {
		t.Parallel()
		_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{}))
		validateExpectedError(t, err)
	})

	t.Run("bidi", func(t *testing.T) {
		t.Parallel()
		bidiStream := client.CumSum(context.Background())
		err := bidiStream.Send(&pingv1.CumSumRequest{})
		validateExpectedError(t, err)
	})

	t.Run("client_stream", func(t *testing.T) {
		t.Parallel()
		clientStream := client.Sum(context.Background())
		err := clientStream.Send(&pingv1.SumRequest{})
		validateExpectedError(t, err)
	})

	t.Run("server_stream", func(t *testing.T) {
		t.Parallel()
		_, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{Number: 3}))
		validateExpectedError(t, err)
	})
}

func TestClientPeer(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	run := func(t *testing.T, opts ...connect.ClientOption) {
		t.Helper()
		client := pingv1connect.NewPingServiceClient(
			server.Client(),
			server.URL,
			connect.WithClientOptions(opts...),
			connect.WithInterceptors(&assertPeerInterceptor{t}),
		)
		ctx := context.Background()
		// unary
		_, err := client.Ping(ctx, connect.NewRequest(&pingv1.PingRequest{}))
		assert.Nil(t, err)
		// client streaming
		clientStream := client.Sum(ctx)
		t.Cleanup(func() {
			_, closeErr := clientStream.CloseAndReceive()
			assert.Nil(t, closeErr)
		})
		assert.NotZero(t, clientStream.Peer().Addr)
		assert.NotZero(t, clientStream.Peer().Protocol)
		err = clientStream.Send(&pingv1.SumRequest{})
		assert.Nil(t, err)
		// server streaming
		serverStream, err := client.CountUp(ctx, connect.NewRequest(&pingv1.CountUpRequest{}))
		t.Cleanup(func() {
			assert.Nil(t, serverStream.Close())
		})
		assert.Nil(t, err)
		// bidi streaming
		bidiStream := client.CumSum(ctx)
		t.Cleanup(func() {
			assert.Nil(t, bidiStream.CloseRequest())
			assert.Nil(t, bidiStream.CloseResponse())
		})
		assert.NotZero(t, bidiStream.Peer().Addr)
		assert.NotZero(t, bidiStream.Peer().Protocol)
		err = bidiStream.Send(&pingv1.CumSumRequest{})
		assert.Nil(t, err)
	}

	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		run(t)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		run(t, connect.WithGRPC())
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		run(t, connect.WithGRPCWeb())
	})
}

func TestConnectHTTPErrorCodes(t *testing.T) {
	t.Parallel()
	checkHTTPStatus := func(t *testing.T, connectCode connect.Code, wantHttpStatus int) {
		t.Helper()
		mux := http.NewServeMux()
		pluggableServer := &pluggablePingServer{
			ping: func(_ context.Context, _ *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
				return nil, connect.NewError(connectCode, errors.New("error"))
			},
		}
		mux.Handle(pingv1connect.NewPingServiceHandler(pluggableServer))
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		req, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodPost,
			server.URL+"/"+pingv1connect.PingServiceName+"/Ping",
			strings.NewReader("{}"),
		)
		assert.Nil(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := server.Client().Do(req)
		assert.Nil(t, err)
		assert.Equal(t, wantHttpStatus, resp.StatusCode)
		defer resp.Body.Close()
		connectClient := pingv1connect.NewPingServiceClient(server.Client(), server.URL)
		connectResp, err := connectClient.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{}))
		assert.NotNil(t, err)
		assert.Nil(t, connectResp)
	}
	t.Run("CodeCanceled-408", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeCanceled, 408)
	})
	t.Run("CodeUnknown-500", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeUnknown, 500)
	})
	t.Run("CodeInvalidArgument-400", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeInvalidArgument, 400)
	})
	t.Run("CodeDeadlineExceeded-408", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeDeadlineExceeded, 408)
	})
	t.Run("CodeNotFound-404", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeNotFound, 404)
	})
	t.Run("CodeAlreadyExists-409", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeAlreadyExists, 409)
	})
	t.Run("CodePermissionDenied-403", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodePermissionDenied, 403)
	})
	t.Run("CodeResourceExhausted-429", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeResourceExhausted, 429)
	})
	t.Run("CodeFailedPrecondition-412", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeFailedPrecondition, 412)
	})
	t.Run("CodeAborted-409", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeAborted, 409)
	})
	t.Run("CodeOutOfRange-400", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeOutOfRange, 400)
	})
	t.Run("CodeUnimplemented-404", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeUnimplemented, 404)
	})
	t.Run("CodeInternal-500", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeInternal, 500)
	})
	t.Run("CodeUnavailable-503", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeUnavailable, 503)
	})
	t.Run("CodeDataLoss-500", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeDataLoss, 500)
	})
	t.Run("CodeUnauthenticated-401", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeUnauthenticated, 401)
	})
	t.Run("100-500", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, 100, 500)
	})
	t.Run("0-500", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, 0, 500)
	})
}

type assertPeerInterceptor struct {
	tb testing.TB
}

func (a *assertPeerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		assert.NotZero(a.tb, req.Peer().Addr)
		assert.NotZero(a.tb, req.Peer().Protocol)
		return next(ctx, req)
	}
}

func (a *assertPeerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		assert.NotZero(a.tb, conn.Peer().Addr)
		assert.NotZero(a.tb, conn.Peer().Protocol)
		assert.NotZero(a.tb, conn.Spec())
		return conn
	}
}

func (a *assertPeerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		assert.NotZero(a.tb, conn.Peer().Addr)
		assert.NotZero(a.tb, conn.Peer().Protocol)
		assert.NotZero(a.tb, conn.Spec())
		return next(ctx, conn)
	}
}
