// Copyright 2021-2023 The Connect Authors
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
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	"connectrpc.com/connect/internal/gen/connect/import/v1/importv1connect"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/internal/memhttp"
	"connectrpc.com/connect/internal/memhttp/memhttptest"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
)

const errorMessage = "oh no"

// The ping server implementation used in the tests returns errors if the
// client doesn't set a header, and the server sets headers and trailers on the
// response.
const (
	headerValue                 = "some header value"
	trailerValue                = "some trailer value"
	clientHeader                = "Connect-Client-Header"
	handlerHeader               = "Connect-Handler-Header"
	handlerTrailer              = "Connect-Handler-Trailer"
	clientMiddlewareErrorHeader = "Connect-Trigger-HTTP-Error"
)

func TestServer(t *testing.T) {
	t.Parallel()
	testPing := func(t *testing.T, client pingv1connect.PingServiceClient) { //nolint:thelper
		t.Run("ping", func(t *testing.T) {
			num := int64(42)
			request := connect.NewRequest(&pingv1.PingRequest{Number: num})
			request.Header().Set(clientHeader, headerValue)
			expect := &pingv1.PingResponse{Number: num}
			response, err := client.Ping(context.Background(), request)
			assert.Nil(t, err)
			assert.Equal(t, response.Msg, expect)
			assert.Equal(t, response.Header().Values(handlerHeader), []string{headerValue})
			assert.Equal(t, response.Trailer().Values(handlerTrailer), []string{trailerValue})
		})
		t.Run("zero_ping", func(t *testing.T) {
			request := connect.NewRequest(&pingv1.PingRequest{})
			request.Header().Set(clientHeader, headerValue)
			response, err := client.Ping(context.Background(), request)
			assert.Nil(t, err)
			var expect pingv1.PingResponse
			assert.Equal(t, response.Msg, &expect)
			assert.Equal(t, response.Header().Values(handlerHeader), []string{headerValue})
			assert.Equal(t, response.Trailer().Values(handlerTrailer), []string{trailerValue})
		})
		t.Run("large_ping", func(t *testing.T) {
			// Using a large payload splits the request and response over multiple
			// packets, ensuring that we're managing HTTP readers and writers
			// correctly.
			if testing.Short() {
				t.Skipf("skipping %s test in short mode", t.Name())
			}
			hellos := strings.Repeat("hello", 1024*1024) // ~5mb
			request := connect.NewRequest(&pingv1.PingRequest{Text: hellos})
			request.Header().Set(clientHeader, headerValue)
			response, err := client.Ping(context.Background(), request)
			assert.Nil(t, err)
			assert.Equal(t, response.Msg.GetText(), hellos)
			assert.Equal(t, response.Header().Values(handlerHeader), []string{headerValue})
			assert.Equal(t, response.Trailer().Values(handlerTrailer), []string{trailerValue})
		})
		t.Run("ping_error", func(t *testing.T) {
			_, err := client.Ping(
				context.Background(),
				connect.NewRequest(&pingv1.PingRequest{}),
			)
			assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument)
		})
		t.Run("ping_timeout", func(t *testing.T) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			request := connect.NewRequest(&pingv1.PingRequest{})
			request.Header().Set(clientHeader, headerValue)
			_, err := client.Ping(ctx, request)
			assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded)
		})
	}
	testSum := func(t *testing.T, client pingv1connect.PingServiceClient) { //nolint:thelper
		t.Run("sum", func(t *testing.T) {
			const (
				upTo   = 10
				expect = 55 // 1+10 + 2+9 + ... + 5+6 = 55
			)
			stream := client.Sum(context.Background())
			stream.RequestHeader().Set(clientHeader, headerValue)
			for i := int64(1); i <= upTo; i++ {
				err := stream.Send(&pingv1.SumRequest{Number: i})
				assert.Nil(t, err, assert.Sprintf("send %d", i))
			}
			response, err := stream.CloseAndReceive()
			assert.Nil(t, err)
			assert.Equal(t, response.Msg.GetSum(), expect)
			assert.Equal(t, response.Header().Values(handlerHeader), []string{headerValue})
			assert.Equal(t, response.Trailer().Values(handlerTrailer), []string{trailerValue})
		})
		t.Run("sum_error", func(t *testing.T) {
			stream := client.Sum(context.Background())
			if err := stream.Send(&pingv1.SumRequest{Number: 1}); err != nil {
				assert.ErrorIs(t, err, io.EOF)
				assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
			}
			_, err := stream.CloseAndReceive()
			assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument)
		})
		t.Run("sum_close_and_receive_without_send", func(t *testing.T) {
			stream := client.Sum(context.Background())
			stream.RequestHeader().Set(clientHeader, headerValue)
			got, err := stream.CloseAndReceive()
			assert.Nil(t, err)
			assert.Equal(t, got.Msg, &pingv1.SumResponse{}) // receive header only stream
			assert.Equal(t, got.Header().Values(handlerHeader), []string{headerValue})
		})
	}
	testCountUp := func(t *testing.T, client pingv1connect.PingServiceClient) { //nolint:thelper
		t.Run("count_up", func(t *testing.T) {
			const upTo = 5
			got := make([]int64, 0, upTo)
			expect := make([]int64, 0, upTo)
			for i := 1; i <= upTo; i++ {
				expect = append(expect, int64(i))
			}
			request := connect.NewRequest(&pingv1.CountUpRequest{Number: upTo})
			request.Header().Set(clientHeader, headerValue)
			stream, err := client.CountUp(context.Background(), request)
			assert.Nil(t, err)
			for stream.Receive() {
				got = append(got, stream.Msg().GetNumber())
			}
			assert.Nil(t, stream.Err())
			assert.Nil(t, stream.Close())
			assert.Equal(t, got, expect)
		})
		t.Run("count_up_error", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			stream, err := client.CountUp(
				ctx,
				connect.NewRequest(&pingv1.CountUpRequest{Number: 1}),
			)
			assert.Nil(t, err)
			for stream.Receive() {
				t.Fatalf("expected error, shouldn't receive any messages")
			}
			assert.Equal(
				t,
				connect.CodeOf(stream.Err()),
				connect.CodeInvalidArgument,
			)
			assert.Nil(t, stream.Close())
		})
		t.Run("count_up_timeout", func(t *testing.T) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			_, err := client.CountUp(ctx, connect.NewRequest(&pingv1.CountUpRequest{Number: 1}))
			assert.NotNil(t, err)
			assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded)
		})
		t.Run("count_up_cancel_after_first_response", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			request := connect.NewRequest(&pingv1.CountUpRequest{Number: 5})
			request.Header().Set(clientHeader, headerValue)
			stream, err := client.CountUp(ctx, request)
			assert.Nil(t, err)
			assert.True(t, stream.Receive())
			cancel()
			assert.False(t, stream.Receive())
			assert.NotNil(t, stream.Err())
			assert.Equal(t, connect.CodeOf(stream.Err()), connect.CodeCanceled)
			assert.Nil(t, stream.Close())
		})
	}
	testCumSum := func(t *testing.T, client pingv1connect.PingServiceClient, expectSuccess bool) { //nolint:thelper
		t.Run("cumsum", func(t *testing.T) {
			send := []int64{3, 5, 1}
			expect := []int64{3, 8, 9}
			var got []int64
			stream := client.CumSum(context.Background())
			stream.RequestHeader().Set(clientHeader, headerValue)
			if !expectSuccess { // server doesn't support HTTP/2
				failNoHTTP2(t, stream)
				return
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for i, n := range send {
					err := stream.Send(&pingv1.CumSumRequest{Number: n})
					assert.Nil(t, err, assert.Sprintf("send error #%d", i))
				}
				assert.Nil(t, stream.CloseRequest())
			}()
			go func() {
				defer wg.Done()
				for {
					msg, err := stream.Receive()
					if errors.Is(err, io.EOF) {
						break
					}
					assert.Nil(t, err)
					got = append(got, msg.GetSum())
				}
				assert.Nil(t, stream.CloseResponse())
			}()
			wg.Wait()
			assert.Equal(t, got, expect)
			assert.Equal(t, stream.ResponseHeader().Values(handlerHeader), []string{headerValue})
			assert.Equal(t, stream.ResponseTrailer().Values(handlerTrailer), []string{trailerValue})
		})
		t.Run("cumsum_error", func(t *testing.T) {
			stream := client.CumSum(context.Background())
			if !expectSuccess { // server doesn't support HTTP/2
				failNoHTTP2(t, stream)
				return
			}
			if err := stream.Send(&pingv1.CumSumRequest{Number: 42}); err != nil {
				assert.ErrorIs(t, err, io.EOF)
				assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
			}
			// We didn't send the headers the server expects, so we should now get an
			// error.
			_, err := stream.Receive()
			assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument)
			assert.True(t, connect.IsWireError(err))
		})
		t.Run("cumsum_empty_stream", func(t *testing.T) {
			stream := client.CumSum(context.Background())
			stream.RequestHeader().Set(clientHeader, headerValue)
			if !expectSuccess { // server doesn't support HTTP/2
				failNoHTTP2(t, stream)
				return
			}
			// Deliberately closing with calling Send to test the behavior of Receive.
			// This test case is based on the grpc interop tests.
			assert.Nil(t, stream.CloseRequest())
			response, err := stream.Receive()
			assert.Nil(t, response)
			assert.True(t, errors.Is(err, io.EOF))
			assert.False(t, connect.IsWireError(err))
			assert.Nil(t, stream.CloseResponse()) // clean-up the stream
		})
		t.Run("cumsum_cancel_after_first_response", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			stream := client.CumSum(ctx)
			stream.RequestHeader().Set(clientHeader, headerValue)
			if !expectSuccess { // server doesn't support HTTP/2
				failNoHTTP2(t, stream)
				cancel()
				return
			}
			var got []int64
			expect := []int64{42}
			if err := stream.Send(&pingv1.CumSumRequest{Number: 42}); err != nil {
				assert.ErrorIs(t, err, io.EOF)
				assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
			}
			msg, err := stream.Receive()
			assert.Nil(t, err)
			got = append(got, msg.GetSum())
			cancel()
			_, err = stream.Receive()
			assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
			assert.Equal(t, got, expect)
			assert.False(t, connect.IsWireError(err))
			assert.Nil(t, stream.CloseResponse())
		})
		t.Run("cumsum_cancel_before_send", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			stream := client.CumSum(ctx)
			if !expectSuccess { // server doesn't support HTTP/2
				failNoHTTP2(t, stream)
				cancel()
				return
			}
			stream.RequestHeader().Set(clientHeader, headerValue)
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 8}))
			cancel()
			// On a subsequent send, ensure that we are still catching context
			// cancellations.
			err := stream.Send(&pingv1.CumSumRequest{Number: 19})
			assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled, assert.Sprintf("%v", err))
			assert.False(t, connect.IsWireError(err))
			assert.Nil(t, stream.CloseRequest())
			assert.Nil(t, stream.CloseResponse())
		})
	}
	testErrors := func(t *testing.T, client pingv1connect.PingServiceClient) { //nolint:thelper
		assertIsHTTPMiddlewareError := func(tb testing.TB, err error) {
			tb.Helper()
			assert.NotNil(tb, err)
			var connectErr *connect.Error
			assert.True(tb, errors.As(err, &connectErr))
			expect := newHTTPMiddlewareError()
			assert.Equal(tb, connectErr.Code(), expect.Code())
			assert.Equal(tb, connectErr.Message(), expect.Message())
			for k, v := range expect.Meta() {
				assert.Equal(tb, connectErr.Meta().Values(k), v)
			}
			assert.Equal(tb, len(connectErr.Details()), len(expect.Details()))
		}
		t.Run("errors", func(t *testing.T) {
			request := connect.NewRequest(&pingv1.FailRequest{
				Code: int32(connect.CodeResourceExhausted),
			})
			request.Header().Set(clientHeader, headerValue)

			response, err := client.Fail(context.Background(), request)
			assert.Nil(t, response)
			assert.NotNil(t, err)
			var connectErr *connect.Error
			ok := errors.As(err, &connectErr)
			assert.True(t, ok, assert.Sprintf("conversion to *connect.Error"))
			assert.True(t, connect.IsWireError(err))
			assert.Equal(t, connectErr.Code(), connect.CodeResourceExhausted)
			assert.Equal(t, connectErr.Error(), "resource_exhausted: "+errorMessage)
			assert.Zero(t, connectErr.Details())
			assert.Equal(t, connectErr.Meta().Values(handlerHeader), []string{headerValue})
			assert.Equal(t, connectErr.Meta().Values(handlerTrailer), []string{trailerValue})
		})
		t.Run("middleware_errors_unary", func(t *testing.T) {
			request := connect.NewRequest(&pingv1.PingRequest{})
			request.Header().Set(clientMiddlewareErrorHeader, headerValue)
			_, err := client.Ping(context.Background(), request)
			assertIsHTTPMiddlewareError(t, err)
		})
		t.Run("middleware_errors_streaming", func(t *testing.T) {
			request := connect.NewRequest(&pingv1.CountUpRequest{Number: 10})
			request.Header().Set(clientMiddlewareErrorHeader, headerValue)
			stream, err := client.CountUp(context.Background(), request)
			assert.Nil(t, err)
			assert.False(t, stream.Receive())
			assertIsHTTPMiddlewareError(t, stream.Err())
		})
	}
	testMatrix := func(t *testing.T, client *http.Client, url string, bidi bool) { //nolint:thelper
		run := func(t *testing.T, opts ...connect.ClientOption) {
			t.Helper()
			client := pingv1connect.NewPingServiceClient(client, url, opts...)
			testPing(t, client)
			testSum(t, client)
			testCountUp(t, client)
			testCumSum(t, client, bidi)
			testErrors(t, client)
		}
		t.Run("connect", func(t *testing.T) {
			t.Run("proto", func(t *testing.T) {
				run(t)
			})
			t.Run("proto_gzip", func(t *testing.T) {
				run(t, connect.WithSendGzip())
			})
			t.Run("json_gzip", func(t *testing.T) {
				run(
					t,
					connect.WithProtoJSON(),
					connect.WithSendGzip(),
				)
			})
		})
		t.Run("grpc", func(t *testing.T) {
			t.Run("proto", func(t *testing.T) {
				run(t, connect.WithGRPC())
			})
			t.Run("proto_gzip", func(t *testing.T) {
				run(t, connect.WithGRPC(), connect.WithSendGzip())
			})
			t.Run("json_gzip", func(t *testing.T) {
				run(
					t,
					connect.WithGRPC(),
					connect.WithProtoJSON(),
					connect.WithSendGzip(),
				)
			})
		})
		t.Run("grpcweb", func(t *testing.T) {
			t.Run("proto", func(t *testing.T) {
				run(t, connect.WithGRPCWeb())
			})
			t.Run("proto_gzip", func(t *testing.T) {
				run(t, connect.WithGRPCWeb(), connect.WithSendGzip())
			})
			t.Run("json_gzip", func(t *testing.T) {
				run(
					t,
					connect.WithGRPCWeb(),
					connect.WithProtoJSON(),
					connect.WithSendGzip(),
				)
			})
		})
	}

	mux := http.NewServeMux()
	pingRoute, pingHandler := pingv1connect.NewPingServiceHandler(
		pingServer{checkMetadata: true},
	)
	errorWriter := connect.NewErrorWriter()
	// Add some net/http middleware to the ping service so we can also exercise ErrorWriter.
	mux.Handle(pingRoute, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get(clientMiddlewareErrorHeader) != "" {
			defer request.Body.Close()
			if _, err := io.Copy(io.Discard, request.Body); err != nil {
				t.Errorf("drain request body: %v", err)
			}
			if !errorWriter.IsSupported(request) {
				t.Errorf("ErrorWriter doesn't support Content-Type %q", request.Header.Get("Content-Type"))
			}
			if err := errorWriter.Write(response, request, newHTTPMiddlewareError()); err != nil {
				t.Errorf("send RPC error from HTTP middleware: %v", err)
			}
			return
		}
		pingHandler.ServeHTTP(response, request)
	}))

	t.Run("http1", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := &http.Client{Transport: server.TransportHTTP1()}
		testMatrix(t, client, server.URL(), false /* bidi */)
	})
	t.Run("http2", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := server.Client()
		testMatrix(t, client, server.URL(), true /* bidi */)
	})
}

func TestConcurrentStreams(t *testing.T) {
	if testing.Short() {
		t.Skipf("skipping %s test in short mode", t.Name())
	}
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}))
	server := memhttptest.NewServer(t, mux)
	var done, start sync.WaitGroup
	start.Add(1)
	for i := 0; i < runtime.GOMAXPROCS(0)*8; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
			var total int64
			sum := client.CumSum(context.Background())
			start.Wait()
			for i := 0; i < 100; i++ {
				num := rand.Int63n(1000) //nolint: gosec
				total += num
				if err := sum.Send(&pingv1.CumSumRequest{Number: num}); err != nil {
					t.Errorf("failed to send request: %v", err)
					break
				}
				resp, err := sum.Receive()
				if err != nil {
					t.Errorf("failed to receive from stream: %v", err)
					break
				}
				if got := resp.GetSum(); total != got {
					t.Errorf("expected %d == %d", total, got)
					break
				}
			}
			if err := sum.CloseRequest(); err != nil {
				t.Errorf("failed to close request: %v", err)
			}
			if err := sum.CloseResponse(); err != nil {
				t.Errorf("failed to close response: %v", err)
			}
		}()
	}
	start.Done()
	done.Wait()
}

func TestHeaderBasic(t *testing.T) {
	t.Parallel()
	const (
		key  = "Test-Key"
		cval = "client value"
		hval = "client value"
	)

	pingServer := &pluggablePingServer{
		ping: func(ctx context.Context, request *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
			assert.Equal(t, request.Header().Get(key), cval)
			response := connect.NewResponse(&pingv1.PingResponse{})
			response.Header().Set(key, hval)
			return response, nil
		},
	}
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer))
	server := memhttptest.NewServer(t, mux)

	client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
	request := connect.NewRequest(&pingv1.PingRequest{})
	request.Header().Set(key, cval)
	response, err := client.Ping(context.Background(), request)
	assert.Nil(t, err)
	assert.Equal(t, response.Header().Get(key), hval)
}

func TestHeaderHost(t *testing.T) {
	t.Parallel()
	const (
		key  = "Host"
		cval = "buf.build"
	)

	pingServer := &pluggablePingServer{
		ping: func(_ context.Context, request *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
			assert.Equal(t, request.Header().Get(key), cval)
			response := connect.NewResponse(&pingv1.PingResponse{})
			return response, nil
		},
	}

	newHTTP2Server := func(t *testing.T) *memhttp.Server {
		t.Helper()
		mux := http.NewServeMux()
		mux.Handle(pingv1connect.NewPingServiceHandler(pingServer))
		server := memhttptest.NewServer(t, mux)
		return server
	}

	callWithHost := func(t *testing.T, client pingv1connect.PingServiceClient) {
		t.Helper()

		request := connect.NewRequest(&pingv1.PingRequest{})
		request.Header().Set(key, cval)
		response, err := client.Ping(context.Background(), request)
		assert.Nil(t, err)
		assert.Equal(t, response.Header().Get(key), "")
	}

	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
		callWithHost(t, client)
	})

	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPC())
		callWithHost(t, client)
	})

	t.Run("grpc-web", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPCWeb())
		callWithHost(t, client)
	})
}

func TestTimeoutParsing(t *testing.T) {
	t.Parallel()
	const timeout = 10 * time.Minute
	pingServer := &pluggablePingServer{
		ping: func(ctx context.Context, request *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
			deadline, ok := ctx.Deadline()
			assert.True(t, ok)
			remaining := time.Until(deadline)
			assert.True(t, remaining > 0)
			assert.True(t, remaining <= timeout)
			return connect.NewResponse(&pingv1.PingResponse{}), nil
		},
	}
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer))
	server := memhttptest.NewServer(t, mux)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
	_, err := client.Ping(ctx, connect.NewRequest(&pingv1.PingRequest{}))
	assert.Nil(t, err)
}

func TestFailCodec(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	server := memhttptest.NewServer(t, handler)
	client := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL(),
		connect.WithCodec(failCodec{}),
	)
	stream := client.CumSum(context.Background())
	err := stream.Send(&pingv1.CumSumRequest{})
	var connectErr *connect.Error
	assert.NotNil(t, err)
	assert.True(t, errors.As(err, &connectErr))
	assert.Equal(t, connectErr.Code(), connect.CodeInternal)
}

func TestContextError(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	server := memhttptest.NewServer(t, handler)
	client := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL(),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := client.CumSum(ctx)
	err := stream.Send(nil)
	var connectErr *connect.Error
	assert.NotNil(t, err)
	assert.True(t, errors.As(err, &connectErr))
	assert.Equal(t, connectErr.Code(), connect.CodeCanceled)
	assert.False(t, connect.IsWireError(err))
}

func TestGRPCMarshalStatusError(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(
		pingServer{},
		connect.WithCodec(failCodec{}),
	))
	server := memhttptest.NewServer(t, mux)

	assertInternalError := func(tb testing.TB, opts ...connect.ClientOption) {
		tb.Helper()
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), opts...)
		request := connect.NewRequest(&pingv1.FailRequest{Code: int32(connect.CodeResourceExhausted)})
		_, err := client.Fail(context.Background(), request)
		tb.Log(err)
		assert.NotNil(t, err)
		var connectErr *connect.Error
		ok := errors.As(err, &connectErr)
		assert.True(t, ok)
		assert.Equal(t, connectErr.Code(), connect.CodeInternal)
		assert.True(
			t,
			strings.HasSuffix(connectErr.Message(), ": boom"),
		)
	}

	// Only applies to gRPC protocols, where we're marshaling the Status protobuf
	// message to binary.
	assertInternalError(t, connect.WithGRPC())
	assertInternalError(t, connect.WithGRPCWeb())
}

func TestGRPCMissingTrailersError(t *testing.T) {
	t.Parallel()

	trimTrailers := func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del("Te")
			handler.ServeHTTP(&trimTrailerWriter{w: w}, r)
		})
	}

	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(
		pingServer{checkMetadata: true},
	))
	server := memhttptest.NewServer(t, trimTrailers(mux))
	client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPC())

	assertErrorNoTrailers := func(t *testing.T, err error) {
		t.Helper()
		assert.NotNil(t, err)
		var connectErr *connect.Error
		ok := errors.As(err, &connectErr)
		assert.True(t, ok)
		assert.Equal(t, connectErr.Code(), connect.CodeInternal)
		assert.True(
			t,
			strings.HasSuffix(connectErr.Message(), "protocol error: no Grpc-Status trailer: unexpected EOF"),
		)
	}

	assertNilOrEOF := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			assert.ErrorIs(t, err, io.EOF)
		}
	}

	t.Run("ping", func(t *testing.T) {
		t.Parallel()
		request := connect.NewRequest(&pingv1.PingRequest{Number: 1, Text: "foobar"})
		_, err := client.Ping(context.Background(), request)
		assertErrorNoTrailers(t, err)
	})
	t.Run("sum", func(t *testing.T) {
		t.Parallel()
		stream := client.Sum(context.Background())
		err := stream.Send(&pingv1.SumRequest{Number: 1})
		assertNilOrEOF(t, err)
		_, err = stream.CloseAndReceive()
		assertErrorNoTrailers(t, err)
	})
	t.Run("count_up", func(t *testing.T) {
		t.Parallel()
		stream, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{Number: 10}))
		assert.Nil(t, err)
		assert.False(t, stream.Receive())
		assertErrorNoTrailers(t, stream.Err())
	})
	t.Run("cumsum", func(t *testing.T) {
		t.Parallel()
		stream := client.CumSum(context.Background())
		assertNilOrEOF(t, stream.Send(&pingv1.CumSumRequest{Number: 10}))
		_, err := stream.Receive()
		assertErrorNoTrailers(t, err)
		assert.Nil(t, stream.CloseResponse())
	})
	t.Run("cumsum_empty_stream", func(t *testing.T) {
		t.Parallel()
		stream := client.CumSum(context.Background())
		assert.Nil(t, stream.CloseRequest())
		response, err := stream.Receive()
		assert.Nil(t, response)
		assertErrorNoTrailers(t, err)
		assert.Nil(t, stream.CloseResponse())
	})
}

func TestUnavailableIfHostInvalid(t *testing.T) {
	t.Parallel()
	client := pingv1connect.NewPingServiceClient(
		http.DefaultClient,
		"https://api.invalid/",
	)
	_, err := client.Ping(
		context.Background(),
		connect.NewRequest(&pingv1.PingRequest{}),
	)
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable)
}

func TestBidiRequiresHTTP2(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.WriteString(w, "hello world")
		assert.Nil(t, err)
	})
	server := memhttptest.NewServer(t, handler)
	client := pingv1connect.NewPingServiceClient(
		&http.Client{Transport: server.TransportHTTP1()},
		server.URL(),
	)
	stream := client.CumSum(context.Background())
	// Stream creates an async request, can error on Send or Receive.
	if err := stream.Send(&pingv1.CumSumRequest{}); err != nil {
		assert.ErrorIs(t, err, io.EOF)
	}
	assert.Nil(t, stream.CloseRequest())
	_, err := stream.Receive()
	assert.NotNil(t, err)
	var connectErr *connect.Error
	assert.True(t, errors.As(err, &connectErr))
	assert.Equal(t, connectErr.Code(), connect.CodeUnimplemented)
	assert.True(
		t,
		strings.HasSuffix(connectErr.Message(), ": bidi streams require at least HTTP/2"),
	)
}

func TestCompressMinBytesClient(t *testing.T) {
	t.Parallel()
	assertContentType := func(tb testing.TB, text, expect string) {
		tb.Helper()
		mux := http.NewServeMux()
		mux.Handle("/", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assert.Equal(tb, request.Header.Get("Content-Encoding"), expect)
		}))
		server := memhttptest.NewServer(t, mux)
		_, err := pingv1connect.NewPingServiceClient(
			server.Client(),
			server.URL(),
			connect.WithSendGzip(),
			connect.WithCompressMinBytes(8),
		).Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{Text: text}))
		assert.Nil(tb, err)
	}
	t.Run("request_uncompressed", func(t *testing.T) {
		t.Parallel()
		assertContentType(t, "ping", "")
	})
	t.Run("request_compressed", func(t *testing.T) {
		t.Parallel()
		assertContentType(t, "pingping", "gzip")
	})

	t.Run("request_uncompressed", func(t *testing.T) {
		t.Parallel()
		assertContentType(t, "ping", "")
	})
	t.Run("request_compressed", func(t *testing.T) {
		t.Parallel()
		assertContentType(t, strings.Repeat("ping", 2), "gzip")
	})
}

func TestCompressMinBytes(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(
		pingServer{},
		connect.WithCompressMinBytes(8),
	))
	server := memhttptest.NewServer(t, mux)
	client := server.Client()

	getPingResponse := func(t *testing.T, pingText string) *http.Response {
		t.Helper()
		request := &pingv1.PingRequest{Text: pingText}
		requestBytes, err := proto.Marshal(request)
		assert.Nil(t, err)
		req, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodPost,
			server.URL()+pingv1connect.PingServicePingProcedure,
			bytes.NewReader(requestBytes),
		)
		assert.Nil(t, err)
		req.Header.Set("Content-Type", "application/proto")
		response, err := client.Do(req)
		assert.Nil(t, err)
		t.Cleanup(func() {
			assert.Nil(t, response.Body.Close())
		})
		return response
	}

	t.Run("response_uncompressed", func(t *testing.T) {
		t.Parallel()
		assert.False(t, getPingResponse(t, "ping").Uncompressed) //nolint:bodyclose
	})

	t.Run("response_compressed", func(t *testing.T) {
		t.Parallel()
		assert.True(t, getPingResponse(t, strings.Repeat("ping", 2)).Uncompressed) //nolint:bodyclose
	})
}

func TestCustomCompression(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	compressionName := "deflate"
	decompressor := func() connect.Decompressor {
		// Need to instantiate with a reader - before decompressing Reset(io.Reader) is called
		return newDeflateReader(strings.NewReader(""))
	}
	compressor := func() connect.Compressor {
		w, err := flate.NewWriter(&strings.Builder{}, flate.DefaultCompression)
		if err != nil {
			t.Fatalf("failed to create flate writer: %v", err)
		}
		return w
	}
	mux.Handle(pingv1connect.NewPingServiceHandler(
		pingServer{},
		connect.WithCompression(compressionName, decompressor, compressor),
	))
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(server.Client(),
		server.URL(),
		connect.WithAcceptCompression(compressionName, decompressor, compressor),
		connect.WithSendCompression(compressionName),
	)
	request := &pingv1.PingRequest{Text: "testing 1..2..3.."}
	response, err := client.Ping(context.Background(), connect.NewRequest(request))
	assert.Nil(t, err)
	assert.Equal(t, response.Msg, &pingv1.PingResponse{Text: request.GetText()})
}

func TestClientWithoutGzipSupport(t *testing.T) {
	// See https://connectrpc.com/connect/pull/349 for why we want to
	// support this. TL;DR is that Microsoft's dapr sidecar can't handle
	// asymmetric compression.
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}))
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(server.Client(),
		server.URL(),
		connect.WithAcceptCompression("gzip", nil, nil),
		connect.WithSendGzip(),
	)
	request := &pingv1.PingRequest{Text: "gzip me!"}
	_, err := client.Ping(context.Background(), connect.NewRequest(request))
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
	assert.True(t, strings.Contains(err.Error(), "unknown compression"))
}

func TestInvalidHeaderTimeout(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}))
	server := memhttptest.NewServer(t, mux)
	getPingResponseWithTimeout := func(t *testing.T, timeout string) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodPost,
			server.URL()+pingv1connect.PingServicePingProcedure,
			strings.NewReader("{}"),
		)
		assert.Nil(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Connect-Timeout-Ms", timeout)
		response, err := server.Client().Do(request)
		assert.Nil(t, err)
		t.Cleanup(func() {
			assert.Nil(t, response.Body.Close())
		})
		return response
	}
	t.Run("timeout_non_numeric", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, getPingResponseWithTimeout(t, "10s").StatusCode, http.StatusBadRequest) //nolint:bodyclose
	})
	t.Run("timeout_out_of_range", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, getPingResponseWithTimeout(t, "12345678901").StatusCode, http.StatusBadRequest) //nolint:bodyclose
	})
}

func TestInterceptorReturnsWrongType(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}))
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
			if _, err := next(ctx, request); err != nil {
				return nil, err
			}
			return connect.NewResponse(&pingv1.CumSumResponse{
				Sum: 1,
			}), nil
		}
	})))
	_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{Text: "hello!"}))
	assert.NotNil(t, err)
	var connectErr *connect.Error
	assert.True(t, errors.As(err, &connectErr))
	assert.Equal(t, connectErr.Code(), connect.CodeInternal)
	assert.True(t, strings.Contains(connectErr.Message(), "unexpected client response type"))
}

func TestHandlerWithReadMaxBytes(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	readMaxBytes := 1024
	mux.Handle(pingv1connect.NewPingServiceHandler(
		pingServer{},
		connect.WithConditionalHandlerOptions(func(spec connect.Spec) []connect.HandlerOption {
			var options []connect.HandlerOption
			if spec.Procedure == pingv1connect.PingServicePingProcedure {
				options = append(options, connect.WithReadMaxBytes(readMaxBytes))
			}
			return options
		}),
	))
	readMaxBytesMatrix := func(t *testing.T, client pingv1connect.PingServiceClient, compressed bool) {
		t.Helper()
		t.Run("equal_read_max", func(t *testing.T) {
			t.Parallel()
			// Serializes to exactly readMaxBytes (1024) - no errors expected
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", 1021)}
			assert.Equal(t, proto.Size(pingRequest), readMaxBytes)
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			assert.Nil(t, err)
		})
		t.Run("read_max_plus_one", func(t *testing.T) {
			t.Parallel()
			// Serializes to readMaxBytes+1 (1025) - expect invalid argument.
			// This will be over the limit after decompression but under with compression.
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", 1022)}
			if compressed {
				compressedSize := gzipCompressedSize(t, pingRequest)
				assert.True(t, compressedSize < readMaxBytes, assert.Sprintf("expected compressed size %d < %d", compressedSize, readMaxBytes))
			}
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			assert.NotNil(t, err, assert.Sprintf("expected non-nil error for large message"))
			assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
			assert.True(t, strings.HasSuffix(err.Error(), fmt.Sprintf("message size %d is larger than configured max %d", proto.Size(pingRequest), readMaxBytes)))
		})
		t.Run("read_max_large", func(t *testing.T) {
			t.Parallel()
			if testing.Short() {
				t.Skipf("skipping %s test in short mode", t.Name())
			}
			// Serializes to much larger than readMaxBytes (5 MiB)
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("abcde", 1024*1024)}
			expectedSize := proto.Size(pingRequest)
			// With gzip request compression, the error should indicate the envelope size (before decompression) is too large.
			if compressed {
				expectedSize = gzipCompressedSize(t, pingRequest)
				assert.True(t, expectedSize > readMaxBytes, assert.Sprintf("expected compressed size %d > %d", expectedSize, readMaxBytes))
			}
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			assert.NotNil(t, err, assert.Sprintf("expected non-nil error for large message"))
			assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
			assert.Equal(t, err.Error(), fmt.Sprintf("resource_exhausted: message size %d is larger than configured max %d", expectedSize, readMaxBytes))
		})
	}
	newHTTP2Server := func(t *testing.T) *memhttp.Server {
		t.Helper()
		server := memhttptest.NewServer(t, mux)
		return server
	}
	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
		readMaxBytesMatrix(t, client, false)
	})
	t.Run("connect_gzip", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithSendGzip())
		readMaxBytesMatrix(t, client, true)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPC())
		readMaxBytesMatrix(t, client, false)
	})
	t.Run("grpc_gzip", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPC(), connect.WithSendGzip())
		readMaxBytesMatrix(t, client, true)
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPCWeb())
		readMaxBytesMatrix(t, client, false)
	})
	t.Run("grpcweb_gzip", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPCWeb(), connect.WithSendGzip())
		readMaxBytesMatrix(t, client, true)
	})
}

func TestHandlerWithHTTPMaxBytes(t *testing.T) {
	// This is similar to Connect's own ReadMaxBytes option, but applied to the
	// whole stream using the stdlib's http.MaxBytesHandler.
	t.Parallel()
	const readMaxBytes = 128
	mux := http.NewServeMux()
	pingRoute, pingHandler := pingv1connect.NewPingServiceHandler(pingServer{})
	mux.Handle(pingRoute, http.MaxBytesHandler(pingHandler, readMaxBytes))
	run := func(t *testing.T, client pingv1connect.PingServiceClient, compressed bool) {
		t.Helper()
		t.Run("below_read_max", func(t *testing.T) {
			t.Parallel()
			_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{}))
			assert.Nil(t, err)
		})
		t.Run("just_above_max", func(t *testing.T) {
			t.Parallel()
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", readMaxBytes*10)}
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			if compressed {
				compressedSize := gzipCompressedSize(t, pingRequest)
				assert.True(t, compressedSize < readMaxBytes, assert.Sprintf("expected compressed size %d < %d", compressedSize, readMaxBytes))
				assert.Nil(t, err)
				return
			}
			assert.NotNil(t, err, assert.Sprintf("expected non-nil error for large message"))
			assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
		})
		t.Run("read_max_large", func(t *testing.T) {
			t.Parallel()
			if testing.Short() {
				t.Skipf("skipping %s test in short mode", t.Name())
			}
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("abcde", 1024*1024)}
			if compressed {
				expectedSize := gzipCompressedSize(t, pingRequest)
				assert.True(t, expectedSize > readMaxBytes, assert.Sprintf("expected compressed size %d > %d", expectedSize, readMaxBytes))
			}
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			assert.NotNil(t, err, assert.Sprintf("expected non-nil error for large message"))
			assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
		})
	}
	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
		run(t, client, false)
	})
	t.Run("connect_gzip", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithSendGzip())
		run(t, client, true)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPC())
		run(t, client, false)
	})
	t.Run("grpc_gzip", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPC(), connect.WithSendGzip())
		run(t, client, true)
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPCWeb())
		run(t, client, false)
	})
	t.Run("grpcweb_gzip", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPCWeb(), connect.WithSendGzip())
		run(t, client, true)
	})
}

func TestClientWithReadMaxBytes(t *testing.T) {
	t.Parallel()
	createServer := func(tb testing.TB, enableCompression bool) *memhttp.Server {
		tb.Helper()
		mux := http.NewServeMux()
		var compressionOption connect.HandlerOption
		if enableCompression {
			compressionOption = connect.WithCompressMinBytes(1)
		} else {
			compressionOption = connect.WithCompressMinBytes(math.MaxInt)
		}
		mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}, compressionOption))
		server := memhttptest.NewServer(t, mux)
		return server
	}
	serverUncompressed := createServer(t, false)
	serverCompressed := createServer(t, true)
	readMaxBytes := 1024
	readMaxBytesMatrix := func(t *testing.T, client pingv1connect.PingServiceClient, compressed bool) {
		t.Helper()
		t.Run("equal_read_max", func(t *testing.T) {
			t.Parallel()
			// Serializes to exactly readMaxBytes (1024) - no errors expected
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", 1021)}
			assert.Equal(t, proto.Size(pingRequest), readMaxBytes)
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			assert.Nil(t, err)
		})
		t.Run("read_max_plus_one", func(t *testing.T) {
			t.Parallel()
			// Serializes to readMaxBytes+1 (1025) - expect resource exhausted.
			// This will be over the limit after decompression but under with compression.
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", 1022)}
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			assert.NotNil(t, err, assert.Sprintf("expected non-nil error for large message"))
			assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
			assert.True(t, strings.HasSuffix(err.Error(), fmt.Sprintf("message size %d is larger than configured max %d", proto.Size(pingRequest), readMaxBytes)))
		})
		t.Run("read_max_large", func(t *testing.T) {
			t.Parallel()
			if testing.Short() {
				t.Skipf("skipping %s test in short mode", t.Name())
			}
			// Serializes to much larger than readMaxBytes (5 MiB)
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("abcde", 1024*1024)}
			expectedSize := proto.Size(pingRequest)
			// With gzip response compression, the error should indicate the envelope size (before decompression) is too large.
			if compressed {
				expectedSize = gzipCompressedSize(t, pingRequest)
				assert.True(t, expectedSize > readMaxBytes, assert.Sprintf("expected compressed size %d > %d", expectedSize, readMaxBytes))
			}
			assert.True(t, expectedSize > readMaxBytes, assert.Sprintf("expected compressed size %d > %d", expectedSize, readMaxBytes))
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			assert.NotNil(t, err, assert.Sprintf("expected non-nil error for large message"))
			assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
			assert.Equal(t, err.Error(), fmt.Sprintf("resource_exhausted: message size %d is larger than configured max %d", expectedSize, readMaxBytes))
		})
	}
	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(serverUncompressed.Client(), serverUncompressed.URL(), connect.WithReadMaxBytes(readMaxBytes))
		readMaxBytesMatrix(t, client, false)
	})
	t.Run("connect_gzip", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(serverCompressed.Client(), serverCompressed.URL(), connect.WithReadMaxBytes(readMaxBytes))
		readMaxBytesMatrix(t, client, true)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(serverUncompressed.Client(), serverUncompressed.URL(), connect.WithReadMaxBytes(readMaxBytes), connect.WithGRPC())
		readMaxBytesMatrix(t, client, false)
	})
	t.Run("grpc_gzip", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(serverCompressed.Client(), serverCompressed.URL(), connect.WithReadMaxBytes(readMaxBytes), connect.WithGRPC())
		readMaxBytesMatrix(t, client, true)
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(serverUncompressed.Client(), serverUncompressed.URL(), connect.WithReadMaxBytes(readMaxBytes), connect.WithGRPCWeb())
		readMaxBytesMatrix(t, client, false)
	})
	t.Run("grpcweb_gzip", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(serverCompressed.Client(), serverCompressed.URL(), connect.WithReadMaxBytes(readMaxBytes), connect.WithGRPCWeb())
		readMaxBytesMatrix(t, client, true)
	})
}

func TestHandlerWithSendMaxBytes(t *testing.T) {
	t.Parallel()
	sendMaxBytes := 1024
	sendMaxBytesMatrix := func(t *testing.T, client pingv1connect.PingServiceClient, compressed bool) {
		t.Helper()
		t.Run("equal_send_max", func(t *testing.T) {
			t.Parallel()
			// Serializes to exactly sendMaxBytes (1024) - no errors expected
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", 1021)}
			assert.Equal(t, proto.Size(pingRequest), sendMaxBytes)
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			assert.Nil(t, err)
		})
		t.Run("send_max_plus_one", func(t *testing.T) {
			t.Parallel()
			// Serializes to sendMaxBytes+1 (1025) - expect invalid argument.
			// This will be over the limit after decompression but under with compression.
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", 1022)}
			if compressed {
				compressedSize := gzipCompressedSize(t, pingRequest)
				assert.True(t, compressedSize < sendMaxBytes, assert.Sprintf("expected compressed size %d < %d", compressedSize, sendMaxBytes))
			}
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			if compressed {
				assert.Nil(t, err)
			} else {
				assert.NotNil(t, err, assert.Sprintf("expected non-nil error for large message"))
				assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
				assert.True(t, strings.HasSuffix(err.Error(), fmt.Sprintf("message size %d exceeds sendMaxBytes %d", proto.Size(pingRequest), sendMaxBytes)))
			}
		})
		t.Run("send_max_large", func(t *testing.T) {
			t.Parallel()
			if testing.Short() {
				t.Skipf("skipping %s test in short mode", t.Name())
			}
			// Serializes to much larger than sendMaxBytes (5 MiB)
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("abcde", 1024*1024)}
			expectedSize := proto.Size(pingRequest)
			// With gzip request compression, the error should indicate the envelope size (before decompression) is too large.
			if compressed {
				expectedSize = gzipCompressedSize(t, pingRequest)
				assert.True(t, expectedSize > sendMaxBytes, assert.Sprintf("expected compressed size %d > %d", expectedSize, sendMaxBytes))
			}
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			assert.NotNil(t, err, assert.Sprintf("expected non-nil error for large message"))
			assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
			if compressed {
				assert.Equal(t, err.Error(), fmt.Sprintf("resource_exhausted: compressed message size %d exceeds sendMaxBytes %d", expectedSize, sendMaxBytes))
			} else {
				assert.Equal(t, err.Error(), fmt.Sprintf("resource_exhausted: message size %d exceeds sendMaxBytes %d", expectedSize, sendMaxBytes))
			}
		})
	}
	newHTTP2Server := func(t *testing.T, compressed bool, sendMaxBytes int) *memhttp.Server {
		t.Helper()
		mux := http.NewServeMux()
		options := []connect.HandlerOption{connect.WithSendMaxBytes(sendMaxBytes)}
		if compressed {
			options = append(options, connect.WithCompressMinBytes(1))
		} else {
			options = append(options, connect.WithCompressMinBytes(math.MaxInt))
		}
		mux.Handle(pingv1connect.NewPingServiceHandler(
			pingServer{},
			options...,
		))
		server := memhttptest.NewServer(t, mux)
		return server
	}
	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t, false, sendMaxBytes)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
		sendMaxBytesMatrix(t, client, false)
	})
	t.Run("connect_gzip", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t, true, sendMaxBytes)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
		sendMaxBytesMatrix(t, client, true)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t, false, sendMaxBytes)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPC())
		sendMaxBytesMatrix(t, client, false)
	})
	t.Run("grpc_gzip", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t, true, sendMaxBytes)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPC())
		sendMaxBytesMatrix(t, client, true)
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t, false, sendMaxBytes)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPCWeb())
		sendMaxBytesMatrix(t, client, false)
	})
	t.Run("grpcweb_gzip", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t, true, sendMaxBytes)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPCWeb())
		sendMaxBytesMatrix(t, client, true)
	})
}

func TestClientWithSendMaxBytes(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}))
	server := memhttptest.NewServer(t, mux)
	sendMaxBytesMatrix := func(t *testing.T, client pingv1connect.PingServiceClient, sendMaxBytes int, compressed bool) {
		t.Helper()
		t.Run("equal_send_max", func(t *testing.T) {
			t.Parallel()
			// Serializes to exactly sendMaxBytes (1024) - no errors expected
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", 1021)}
			assert.Equal(t, proto.Size(pingRequest), sendMaxBytes)
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			assert.Nil(t, err)
		})
		t.Run("send_max_plus_one", func(t *testing.T) {
			t.Parallel()
			// Serializes to sendMaxBytes+1 (1025) - expect resource exhausted.
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", 1022)}
			assert.Equal(t, proto.Size(pingRequest), sendMaxBytes+1)
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			if compressed {
				assert.True(t, gzipCompressedSize(t, pingRequest) < sendMaxBytes)
				assert.Nil(t, err, assert.Sprintf("expected nil error for compressed message < sendMaxBytes"))
			} else {
				assert.NotNil(t, err, assert.Sprintf("expected non-nil error for large message"))
				assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
				assert.True(t, strings.HasSuffix(err.Error(), fmt.Sprintf("message size %d exceeds sendMaxBytes %d", proto.Size(pingRequest), sendMaxBytes)))
			}
		})
		t.Run("send_max_large", func(t *testing.T) {
			t.Parallel()
			if testing.Short() {
				t.Skipf("skipping %s test in short mode", t.Name())
			}
			// Serializes to much larger than sendMaxBytes (5 MiB)
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("abcde", 1024*1024)}
			expectedSize := proto.Size(pingRequest)
			// With gzip response compression, the error should indicate the envelope size (before decompression) is too large.
			if compressed {
				expectedSize = gzipCompressedSize(t, pingRequest)
			}
			assert.True(t, expectedSize > sendMaxBytes)
			_, err := client.Ping(context.Background(), connect.NewRequest(pingRequest))
			assert.NotNil(t, err, assert.Sprintf("expected non-nil error for large message"))
			assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
			if compressed {
				assert.Equal(t, err.Error(), fmt.Sprintf("resource_exhausted: compressed message size %d exceeds sendMaxBytes %d", expectedSize, sendMaxBytes))
			} else {
				assert.Equal(t, err.Error(), fmt.Sprintf("resource_exhausted: message size %d exceeds sendMaxBytes %d", expectedSize, sendMaxBytes))
			}
		})
	}
	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		sendMaxBytes := 1024
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithSendMaxBytes(sendMaxBytes))
		sendMaxBytesMatrix(t, client, sendMaxBytes, false)
	})
	t.Run("connect_gzip", func(t *testing.T) {
		t.Parallel()
		sendMaxBytes := 1024
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithSendMaxBytes(sendMaxBytes), connect.WithSendGzip())
		sendMaxBytesMatrix(t, client, sendMaxBytes, true)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		sendMaxBytes := 1024
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithSendMaxBytes(sendMaxBytes), connect.WithGRPC())
		sendMaxBytesMatrix(t, client, sendMaxBytes, false)
	})
	t.Run("grpc_gzip", func(t *testing.T) {
		t.Parallel()
		sendMaxBytes := 1024
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithSendMaxBytes(sendMaxBytes), connect.WithGRPC(), connect.WithSendGzip())
		sendMaxBytesMatrix(t, client, sendMaxBytes, true)
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		sendMaxBytes := 1024
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithSendMaxBytes(sendMaxBytes), connect.WithGRPCWeb())
		sendMaxBytesMatrix(t, client, sendMaxBytes, false)
	})
	t.Run("grpcweb_gzip", func(t *testing.T) {
		t.Parallel()
		sendMaxBytes := 1024
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithSendMaxBytes(sendMaxBytes), connect.WithGRPCWeb(), connect.WithSendGzip())
		sendMaxBytesMatrix(t, client, sendMaxBytes, true)
	})
}

func TestBidiStreamServerSendsFirstMessage(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, opts ...connect.ClientOption) {
		t.Helper()
		headersSent := make(chan struct{})
		pingServer := &pluggablePingServer{
			cumSum: func(ctx context.Context, stream *connect.BidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse]) error {
				close(headersSent)
				return nil
			},
		}
		mux := http.NewServeMux()
		mux.Handle(pingv1connect.NewPingServiceHandler(pingServer))
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(
			server.Client(),
			server.URL(),
			connect.WithClientOptions(opts...),
			connect.WithInterceptors(&assertPeerInterceptor{t}),
		)
		stream := client.CumSum(context.Background())
		t.Cleanup(func() {
			assert.Nil(t, stream.CloseRequest())
			assert.Nil(t, stream.CloseResponse())
		})
		assert.Nil(t, stream.Send(nil))
		select {
		case <-time.After(time.Second):
			t.Error("timed out to get request headers")
		case <-headersSent:
		}
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

func TestStreamForServer(t *testing.T) {
	t.Parallel()
	newPingClient := func(t *testing.T, pingServer pingv1connect.PingServiceHandler) pingv1connect.PingServiceClient {
		t.Helper()
		mux := http.NewServeMux()
		mux.Handle(pingv1connect.NewPingServiceHandler(pingServer))
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(
			server.Client(),
			server.URL(),
		)
		return client
	}
	t.Run("not-proto-message", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			cumSum: func(ctx context.Context, stream *connect.BidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse]) error {
				return stream.Conn().Send("foobar")
			},
		})
		stream := client.CumSum(context.Background())
		assert.Nil(t, stream.Send(nil))
		_, err := stream.Receive()
		assert.NotNil(t, err)
		assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
		assert.Nil(t, stream.CloseRequest())
	})
	t.Run("nil-message", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			cumSum: func(ctx context.Context, stream *connect.BidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse]) error {
				return stream.Send(nil)
			},
		})
		stream := client.CumSum(context.Background())
		assert.Nil(t, stream.Send(nil))
		_, err := stream.Receive()
		assert.NotNil(t, err)
		assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
		assert.Nil(t, stream.CloseRequest())
	})
	t.Run("get-spec", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			cumSum: func(ctx context.Context, stream *connect.BidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse]) error {
				assert.Equal(t, stream.Spec().StreamType, connect.StreamTypeBidi)
				assert.Equal(t, stream.Spec().Procedure, pingv1connect.PingServiceCumSumProcedure)
				assert.False(t, stream.Spec().IsClient)
				return nil
			},
		})
		stream := client.CumSum(context.Background())
		assert.Nil(t, stream.Send(nil))
		assert.Nil(t, stream.CloseRequest())
	})
	t.Run("server-stream", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			countUp: func(ctx context.Context, req *connect.Request[pingv1.CountUpRequest], stream *connect.ServerStream[pingv1.CountUpResponse]) error {
				assert.Equal(t, stream.Conn().Spec().StreamType, connect.StreamTypeServer)
				assert.Equal(t, stream.Conn().Spec().Procedure, pingv1connect.PingServiceCountUpProcedure)
				assert.False(t, stream.Conn().Spec().IsClient)
				assert.Nil(t, stream.Send(&pingv1.CountUpResponse{Number: 1}))
				return nil
			},
		})
		stream, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{}))
		assert.Nil(t, err)
		assert.NotNil(t, stream)
		assert.Nil(t, stream.Close())
	})
	t.Run("server-stream-send", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			countUp: func(ctx context.Context, req *connect.Request[pingv1.CountUpRequest], stream *connect.ServerStream[pingv1.CountUpResponse]) error {
				assert.Nil(t, stream.Send(&pingv1.CountUpResponse{Number: 1}))
				return nil
			},
		})
		stream, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{}))
		assert.Nil(t, err)
		assert.True(t, stream.Receive())
		msg := stream.Msg()
		assert.NotNil(t, msg)
		assert.Equal(t, msg.GetNumber(), 1)
		assert.Nil(t, stream.Close())
	})
	t.Run("server-stream-send-nil", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			countUp: func(ctx context.Context, req *connect.Request[pingv1.CountUpRequest], stream *connect.ServerStream[pingv1.CountUpResponse]) error {
				stream.ResponseHeader().Set("foo", "bar")
				stream.ResponseTrailer().Set("bas", "blah")
				assert.Nil(t, stream.Send(nil))
				return nil
			},
		})
		stream, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{}))
		assert.Nil(t, err)
		assert.False(t, stream.Receive())
		headers := stream.ResponseHeader()
		assert.NotNil(t, headers)
		assert.Equal(t, headers.Get("foo"), "bar")
		trailers := stream.ResponseTrailer()
		assert.NotNil(t, trailers)
		assert.Equal(t, trailers.Get("bas"), "blah")
		assert.Nil(t, stream.Close())
	})
	t.Run("client-stream", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			sum: func(ctx context.Context, stream *connect.ClientStream[pingv1.SumRequest]) (*connect.Response[pingv1.SumResponse], error) {
				assert.Equal(t, stream.Spec().StreamType, connect.StreamTypeClient)
				assert.Equal(t, stream.Spec().Procedure, pingv1connect.PingServiceSumProcedure)
				assert.False(t, stream.Spec().IsClient)
				assert.True(t, stream.Receive())
				msg := stream.Msg()
				assert.NotNil(t, msg)
				assert.Equal(t, msg.GetNumber(), 1)
				return connect.NewResponse(&pingv1.SumResponse{Sum: 1}), nil
			},
		})
		stream := client.Sum(context.Background())
		assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 1}))
		res, err := stream.CloseAndReceive()
		assert.Nil(t, err)
		assert.NotNil(t, res)
		assert.Equal(t, res.Msg.GetSum(), 1)
	})
	t.Run("client-stream-conn", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			sum: func(ctx context.Context, stream *connect.ClientStream[pingv1.SumRequest]) (*connect.Response[pingv1.SumResponse], error) {
				assert.True(t, stream.Receive())
				assert.NotNil(t, stream.Conn().Send("not-proto"))
				return connect.NewResponse(&pingv1.SumResponse{}), nil
			},
		})
		stream := client.Sum(context.Background())
		assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 1}))
		res, err := stream.CloseAndReceive()
		assert.Nil(t, err)
		assert.NotNil(t, res)
	})
	t.Run("client-stream-send-msg", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			sum: func(ctx context.Context, stream *connect.ClientStream[pingv1.SumRequest]) (*connect.Response[pingv1.SumResponse], error) {
				assert.True(t, stream.Receive())
				assert.Nil(t, stream.Conn().Send(&pingv1.SumResponse{Sum: 2}))
				return connect.NewResponse(&pingv1.SumResponse{}), nil
			},
		})
		stream := client.Sum(context.Background())
		assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 1}))
		res, err := stream.CloseAndReceive()
		assert.NotNil(t, err)
		assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
		assert.Nil(t, res)
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
		server := memhttptest.NewServer(t, mux)
		req, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodPost,
			server.URL()+pingv1connect.PingServicePingProcedure,
			strings.NewReader("{}"),
		)
		assert.Nil(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := server.Client().Do(req)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, wantHttpStatus, resp.StatusCode)
		connectClient := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
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

func TestFailCompression(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	compressorName := "fail"
	compressor := func() connect.Compressor { return failCompressor{} }
	decompressor := func() connect.Decompressor { return failDecompressor{} }
	mux.Handle(
		pingv1connect.NewPingServiceHandler(
			pingServer{},
			connect.WithCompression(compressorName, decompressor, compressor),
		),
	)
	server := memhttptest.NewServer(t, mux)
	pingclient := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL(),
		connect.WithAcceptCompression(compressorName, decompressor, compressor),
		connect.WithSendCompression(compressorName),
	)
	_, err := pingclient.Ping(
		context.Background(),
		connect.NewRequest(&pingv1.PingRequest{
			Text: "ping",
		}),
	)
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
}

func TestUnflushableResponseWriter(t *testing.T) {
	t.Parallel()
	assertIsFlusherErr := func(t *testing.T, err error) {
		t.Helper()
		assert.NotNil(t, err)
		assert.Equal(t, connect.CodeOf(err), connect.CodeInternal, assert.Sprintf("got %v", err))
		assert.True(
			t,
			strings.HasSuffix(err.Error(), "unflushableWriter does not implement http.Flusher"),
			assert.Sprintf("error doesn't reference http.Flusher: %s", err.Error()),
		)
	}
	mux := http.NewServeMux()
	path, handler := pingv1connect.NewPingServiceHandler(pingServer{})
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(&unflushableWriter{w}, r)
	})
	mux.Handle(path, wrapped)
	server := memhttptest.NewServer(t, mux)

	tests := []struct {
		name    string
		options []connect.ClientOption
	}{
		{"connect", nil},
		{"grpc", []connect.ClientOption{connect.WithGRPC()}},
		{"grpcweb", []connect.ClientOption{connect.WithGRPCWeb()}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pingclient := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), tt.options...)
			stream, err := pingclient.CountUp(
				context.Background(),
				connect.NewRequest(&pingv1.CountUpRequest{Number: 5}),
			)
			if err != nil {
				assertIsFlusherErr(t, err)
				return
			}
			assert.False(t, stream.Receive())
			assertIsFlusherErr(t, stream.Err())
		})
	}
}

func TestGRPCErrorMetadataIsTrailersOnly(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}))
	server := memhttptest.NewServer(t, mux)

	protoBytes, err := proto.Marshal(&pingv1.FailRequest{Code: int32(connect.CodeInternal)})
	assert.Nil(t, err)
	// Manually construct a gRPC prefix. Data is uncompressed, so the first byte
	// is 0. Set the last 4 bytes to the message length.
	var prefix [5]byte
	binary.BigEndian.PutUint32(prefix[1:5], uint32(len(protoBytes)))
	body := append(prefix[:], protoBytes...)
	// Manually send off a gRPC request.
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		server.URL()+pingv1connect.PingServiceFailProcedure,
		bytes.NewReader(body),
	)
	assert.Nil(t, err)
	req.Header.Set("Content-Type", "application/grpc")
	res, err := server.Client().Do(req)
	assert.Nil(t, err)
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, res.Header.Get("Content-Type"), "application/grpc")
	// pingServer.Fail adds handlerHeader and handlerTrailer to the error
	// metadata. The gRPC protocol should send all error metadata as trailers.
	assert.Zero(t, res.Header.Get(handlerHeader))
	assert.Zero(t, res.Header.Get(handlerTrailer))
	_, err = io.Copy(io.Discard, res.Body)
	assert.Nil(t, err)
	assert.Nil(t, res.Body.Close())
	assert.NotZero(t, res.Trailer.Get(handlerHeader))
	assert.NotZero(t, res.Trailer.Get(handlerTrailer))
}

func TestConnectProtocolHeaderSentByDefault(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}, connect.WithRequireConnectProtocolHeader()))
	server := memhttptest.NewServer(t, mux)

	client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
	_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{}))
	assert.Nil(t, err)

	stream := client.CumSum(context.Background())
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{}))
	_, err = stream.Receive()
	assert.Nil(t, err)
	assert.Nil(t, stream.CloseRequest())
	assert.Nil(t, stream.CloseResponse())
}

func TestConnectProtocolHeaderRequired(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(
		pingServer{},
		connect.WithRequireConnectProtocolHeader(),
	))
	server := memhttptest.NewServer(t, mux)

	tests := []struct {
		headers http.Header
	}{
		{http.Header{}},
		{http.Header{"Connect-Protocol-Version": []string{"0"}}},
	}
	for _, tcase := range tests {
		req, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodPost,
			server.URL()+pingv1connect.PingServicePingProcedure,
			strings.NewReader("{}"),
		)
		assert.Nil(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range tcase.headers {
			req.Header[k] = v
		}
		response, err := server.Client().Do(req)
		assert.Nil(t, err)
		assert.Nil(t, response.Body.Close())
		assert.Equal(t, response.StatusCode, http.StatusBadRequest)
	}
}

func TestAllowCustomUserAgent(t *testing.T) {
	t.Parallel()

	const customAgent = "custom"
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(&pluggablePingServer{
		ping: func(_ context.Context, req *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
			agent := req.Header().Get("User-Agent")
			assert.Equal(t, agent, customAgent)
			return connect.NewResponse(&pingv1.PingResponse{Number: req.Msg.GetNumber()}), nil
		},
	}))
	server := memhttptest.NewServer(t, mux)

	// If the user has set a User-Agent, we shouldn't clobber it.
	tests := []struct {
		protocol string
		opts     []connect.ClientOption
	}{
		{"connect", nil},
		{"grpc", []connect.ClientOption{connect.WithGRPC()}},
		{"grpcweb", []connect.ClientOption{connect.WithGRPCWeb()}},
	}
	for _, testCase := range tests {
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), testCase.opts...)
		req := connect.NewRequest(&pingv1.PingRequest{Number: 42})
		req.Header().Set("User-Agent", customAgent)
		_, err := client.Ping(context.Background(), req)
		assert.Nil(t, err)
	}
}

func TestWebXUserAgent(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(&pluggablePingServer{
		ping: func(_ context.Context, req *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
			agent := req.Header().Get("User-Agent")
			assert.NotZero(t, agent)
			assert.Equal(
				t,
				req.Header().Get("X-User-Agent"),
				agent,
			)
			return connect.NewResponse(&pingv1.PingResponse{Number: req.Msg.GetNumber()}), nil
		},
	}))
	server := memhttptest.NewServer(t, mux)

	client := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithGRPCWeb())
	req := connect.NewRequest(&pingv1.PingRequest{Number: 42})
	_, err := client.Ping(context.Background(), req)
	assert.Nil(t, err)
}

func TestBidiOverHTTP1(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}))
	server := memhttptest.NewServer(t, mux)

	// Clients expecting a full-duplex connection that end up with a simplex
	// HTTP/1.1 connection shouldn't hang. Instead, the server should close the
	// TCP connection.
	client := pingv1connect.NewPingServiceClient(
		&http.Client{Transport: server.TransportHTTP1()},
		server.URL(),
	)
	stream := client.CumSum(context.Background())
	// Stream creates an async request, can error on Send or Receive.
	if err := stream.Send(&pingv1.CumSumRequest{Number: 2}); err != nil {
		assert.ErrorIs(t, err, io.EOF)
	}
	_, err := stream.Receive()
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
	assert.Equal(t, err.Error(), "unknown: HTTP status 505 HTTP Version Not Supported")
	assert.Nil(t, stream.CloseRequest())
	assert.Nil(t, stream.CloseResponse())
}

func TestHandlerReturnsNilResponse(t *testing.T) {
	// When user-written handlers return nil responses _and_ nil errors, ensure
	// that the resulting panic includes at least the name of the procedure.
	t.Parallel()

	var panics int
	recoverPanic := func(_ context.Context, spec connect.Spec, _ http.Header, p any) error {
		panics++
		assert.NotNil(t, p)
		str := fmt.Sprint(p)
		assert.True(
			t,
			strings.Contains(str, spec.Procedure),
			assert.Sprintf("%q does not contain procedure %q", str, spec.Procedure),
		)
		return connect.NewError(connect.CodeInternal, errors.New(str))
	}

	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(&pluggablePingServer{
		ping: func(ctx context.Context, req *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
			return nil, nil //nolint: nilnil
		},
		sum: func(ctx context.Context, req *connect.ClientStream[pingv1.SumRequest]) (*connect.Response[pingv1.SumResponse], error) {
			return nil, nil //nolint: nilnil
		},
	}, connect.WithRecover(recoverPanic)))
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())

	_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{}))
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)

	_, err = client.Sum(context.Background()).CloseAndReceive()
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)

	assert.Equal(t, panics, 2)
}

func TestStreamUnexpectedEOF(t *testing.T) {
	t.Parallel()

	// Initialized by the test case.
	testcaseMux := make(map[string]http.HandlerFunc)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(responseWriter http.ResponseWriter, request *http.Request) {
		testcase, ok := testcaseMux[request.Header.Get("Test-Case")]
		if !ok {
			responseWriter.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.Copy(io.Discard, request.Body)
		testcase(responseWriter, request)
	})
	server := memhttptest.NewServer(t, mux)

	head := [5]byte{}
	payload := []byte(`{"number": 42}`)
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	testcases := []struct {
		name       string
		handler    http.HandlerFunc
		options    []connect.ClientOption
		expectCode connect.Code
		expectMsg  string
	}{{
		name:    "connect_missing_end",
		options: []connect.ClientOption{connect.WithProtoJSON()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			header := responseWriter.Header()
			header.Set("Content-Type", "application/connect+json")
			_, err := responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write(payload)
			assert.Nil(t, err)
		},
		expectCode: connect.CodeInternal,
		expectMsg:  "internal: protocol error: unexpected EOF",
	}, {
		name:    "grpc_missing_end",
		options: []connect.ClientOption{connect.WithProtoJSON(), connect.WithGRPC()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			header := responseWriter.Header()
			header.Set("Content-Type", "application/grpc+json")
			_, err := responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write(payload)
			assert.Nil(t, err)
		},
		expectCode: connect.CodeInternal,
		expectMsg:  "internal: protocol error: no Grpc-Status trailer: unexpected EOF",
	}, {
		name:    "grpc-web_missing_end",
		options: []connect.ClientOption{connect.WithProtoJSON(), connect.WithGRPCWeb()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			header := responseWriter.Header()
			header.Set("Content-Type", "application/grpc-web+json")
			_, err := responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, _ = responseWriter.Write(payload)
			assert.Nil(t, err)
		},
		expectCode: connect.CodeInternal,
		expectMsg:  "internal: protocol error: no Grpc-Status trailer: unexpected EOF",
	}, {
		name:    "connect_partial_payload",
		options: []connect.ClientOption{connect.WithProtoJSON()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			header := responseWriter.Header()
			header.Set("Content-Type", "application/connect+json")
			_, err := responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write(payload[:len(payload)-1])
			assert.Nil(t, err)
		},
		expectCode: connect.CodeInvalidArgument,
		expectMsg:  fmt.Sprintf("invalid_argument: protocol error: promised %d bytes in enveloped message, got %d bytes", len(payload), len(payload)-1),
	}, {
		name:    "grpc_partial_payload",
		options: []connect.ClientOption{connect.WithProtoJSON(), connect.WithGRPC()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			header := responseWriter.Header()
			header.Set("Content-Type", "application/grpc+json")
			_, err := responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write(payload[:len(payload)-1])
			assert.Nil(t, err)
		},
		expectCode: connect.CodeInvalidArgument,
		expectMsg:  fmt.Sprintf("invalid_argument: protocol error: promised %d bytes in enveloped message, got %d bytes", len(payload), len(payload)-1),
	}, {
		name:    "grpc-web_partial_payload",
		options: []connect.ClientOption{connect.WithProtoJSON(), connect.WithGRPCWeb()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			header := responseWriter.Header()
			header.Set("Content-Type", "application/grpc-web+json")
			_, err := responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write(payload[:len(payload)-1])
			assert.Nil(t, err)
		},
		expectCode: connect.CodeInvalidArgument,
		expectMsg:  fmt.Sprintf("invalid_argument: protocol error: promised %d bytes in enveloped message, got %d bytes", len(payload), len(payload)-1),
	}, {
		name:    "connect_partial_frame",
		options: []connect.ClientOption{connect.WithProtoJSON()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			header := responseWriter.Header()
			header.Set("Content-Type", "application/connect+json")
			_, err := responseWriter.Write(head[:4])
			assert.Nil(t, err)
		},
		expectCode: connect.CodeInvalidArgument,
		expectMsg:  "invalid_argument: protocol error: incomplete envelope: unexpected EOF",
	}, {
		name:    "grpc_partial_frame",
		options: []connect.ClientOption{connect.WithProtoJSON(), connect.WithGRPC()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			header := responseWriter.Header()
			header.Set("Content-Type", "application/grpc+json")
			_, err := responseWriter.Write(head[:4])
			assert.Nil(t, err)
		},
		expectCode: connect.CodeInvalidArgument,
		expectMsg:  "invalid_argument: protocol error: incomplete envelope: unexpected EOF",
	}, {
		name:    "grpc-web_partial_frame",
		options: []connect.ClientOption{connect.WithProtoJSON(), connect.WithGRPCWeb()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			header := responseWriter.Header()
			header.Set("Content-Type", "application/grpc-web+json")
			_, err := responseWriter.Write(head[:4])
			assert.Nil(t, err)
		},
		expectCode: connect.CodeInvalidArgument,
		expectMsg:  "invalid_argument: protocol error: incomplete envelope: unexpected EOF",
	}, {
		name:    "connect_excess_eof",
		options: []connect.ClientOption{connect.WithProtoJSON()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			_, err := responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write(payload)
			assert.Nil(t, err)
			// Write EOF
			_, err = responseWriter.Write([]byte{1 << 1, 0, 0, 0, 2})
			assert.Nil(t, err)
			_, err = responseWriter.Write([]byte("{}"))
			assert.Nil(t, err)
			// Excess payload
			_, err = responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write(payload)
			assert.Nil(t, err)
		},
		expectCode: connect.CodeInternal,
		expectMsg:  fmt.Sprintf("internal: corrupt response: %d extra bytes after end of stream", len(payload)+len(head)),
	}, {
		name:    "grpc-web_excess_eof",
		options: []connect.ClientOption{connect.WithProtoJSON(), connect.WithGRPCWeb()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			_, err := responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write(payload)
			assert.Nil(t, err)
			// Write EOF
			var buf bytes.Buffer
			trailer := http.Header{"grpc-status": []string{"0"}}
			assert.Nil(t, trailer.Write(&buf))
			var head [5]byte
			head[0] = 1 << 7
			binary.BigEndian.PutUint32(head[1:], uint32(buf.Len()))
			_, err = responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write(buf.Bytes())
			assert.Nil(t, err)
			// Excess payload
			_, err = responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write(payload)
			assert.Nil(t, err)
		},
		expectCode: connect.CodeInternal,
		expectMsg:  fmt.Sprintf("internal: corrupt response: %d extra bytes after end of stream", len(payload)+len(head)),
	}}
	for _, testcase := range testcases {
		testcaseMux[t.Name()+"/"+testcase.name] = testcase.handler
	}
	for _, testcase := range testcases {
		testcase := testcase
		t.Run(testcase.name, func(t *testing.T) {
			t.Parallel()
			client := pingv1connect.NewPingServiceClient(
				server.Client(),
				server.URL(),
				testcase.options...,
			)
			const upTo = 2
			request := connect.NewRequest(&pingv1.CountUpRequest{Number: upTo})
			request.Header().Set("Test-Case", t.Name())
			stream, err := client.CountUp(context.Background(), request)
			assert.Nil(t, err)
			for i := 0; stream.Receive() && i < upTo; i++ {
				assert.Equal(t, stream.Msg().GetNumber(), 42)
			}
			assert.NotNil(t, stream.Err())
			assert.Equal(t, connect.CodeOf(stream.Err()), testcase.expectCode)
			assert.Equal(t, stream.Err().Error(), testcase.expectMsg)
		})
	}
}

// TestBlankImportCodeGeneration tests that services.connect.go is generated with
// blank import statements to services.pb.go so that the service's Descriptor is
// available in the global proto registry.
func TestBlankImportCodeGeneration(t *testing.T) {
	t.Parallel()
	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(importv1connect.ImportServiceName)
	assert.Nil(t, err)
	assert.NotNil(t, desc)
}

type unflushableWriter struct {
	w http.ResponseWriter
}

func (w *unflushableWriter) Header() http.Header         { return w.w.Header() }
func (w *unflushableWriter) Write(b []byte) (int, error) { return w.w.Write(b) }
func (w *unflushableWriter) WriteHeader(code int)        { w.w.WriteHeader(code) }

func gzipCompressedSize(tb testing.TB, message proto.Message) int {
	tb.Helper()
	uncompressed, err := proto.Marshal(message)
	assert.Nil(tb, err)
	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	_, err = gzipWriter.Write(uncompressed)
	assert.Nil(tb, err)
	assert.Nil(tb, gzipWriter.Close())
	return buf.Len()
}

type failCodec struct{}

func (c failCodec) Name() string {
	return "proto"
}

func (c failCodec) Marshal(message any) ([]byte, error) {
	return nil, errors.New("boom")
}

func (c failCodec) Unmarshal(data []byte, message any) error {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return fmt.Errorf("not protobuf: %T", message)
	}
	return proto.Unmarshal(data, protoMessage)
}

type pluggablePingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	ping    func(context.Context, *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error)
	sum     func(context.Context, *connect.ClientStream[pingv1.SumRequest]) (*connect.Response[pingv1.SumResponse], error)
	countUp func(context.Context, *connect.Request[pingv1.CountUpRequest], *connect.ServerStream[pingv1.CountUpResponse]) error
	cumSum  func(context.Context, *connect.BidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse]) error
}

func (p *pluggablePingServer) Ping(
	ctx context.Context,
	request *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {
	return p.ping(ctx, request)
}

func (p *pluggablePingServer) Sum(
	ctx context.Context,
	stream *connect.ClientStream[pingv1.SumRequest],
) (*connect.Response[pingv1.SumResponse], error) {
	return p.sum(ctx, stream)
}

func (p *pluggablePingServer) CountUp(
	ctx context.Context,
	req *connect.Request[pingv1.CountUpRequest],
	stream *connect.ServerStream[pingv1.CountUpResponse],
) error {
	return p.countUp(ctx, req, stream)
}

func (p *pluggablePingServer) CumSum(
	ctx context.Context,
	stream *connect.BidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse],
) error {
	return p.cumSum(ctx, stream)
}

func failNoHTTP2(tb testing.TB, stream *connect.BidiStreamForClient[pingv1.CumSumRequest, pingv1.CumSumResponse]) {
	tb.Helper()

	if err := stream.Send(&pingv1.CumSumRequest{}); err != nil {
		assert.ErrorIs(tb, err, io.EOF)
		assert.Equal(tb, connect.CodeOf(err), connect.CodeUnknown)
	}
	assert.Nil(tb, stream.CloseRequest())
	_, err := stream.Receive()
	assert.NotNil(tb, err) // should be 505
	assert.True(
		tb,
		strings.Contains(err.Error(), "HTTP status 505"),
		assert.Sprintf("expected 505, got %v", err),
	)
	assert.Nil(tb, stream.CloseResponse())
}

func expectClientHeader(check bool, req connect.AnyRequest) error {
	if !check {
		return nil
	}
	return expectMetadata(req.Header(), "header", clientHeader, headerValue)
}

func expectMetadata(meta http.Header, metaType, key, value string) error {
	if got := meta.Get(key); got != value {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"%s %q: got %q, expected %q",
			metaType,
			key,
			got,
			value,
		))
	}
	return nil
}

type pingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	checkMetadata bool
}

func (p pingServer) Ping(ctx context.Context, request *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
	if err := expectClientHeader(p.checkMetadata, request); err != nil {
		return nil, err
	}
	if request.Peer().Addr == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("no peer address"))
	}
	if request.Peer().Protocol == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("no peer protocol"))
	}
	response := connect.NewResponse(
		&pingv1.PingResponse{
			Number: request.Msg.GetNumber(),
			Text:   request.Msg.GetText(),
		},
	)
	response.Header().Set(handlerHeader, headerValue)
	response.Trailer().Set(handlerTrailer, trailerValue)
	return response, nil
}

func (p pingServer) Fail(ctx context.Context, request *connect.Request[pingv1.FailRequest]) (*connect.Response[pingv1.FailResponse], error) {
	if err := expectClientHeader(p.checkMetadata, request); err != nil {
		return nil, err
	}
	if request.Peer().Addr == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("no peer address"))
	}
	if request.Peer().Protocol == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("no peer protocol"))
	}
	err := connect.NewError(connect.Code(request.Msg.GetCode()), errors.New(errorMessage))
	err.Meta().Set(handlerHeader, headerValue)
	err.Meta().Set(handlerTrailer, trailerValue)
	return nil, err
}

func (p pingServer) Sum(
	ctx context.Context,
	stream *connect.ClientStream[pingv1.SumRequest],
) (*connect.Response[pingv1.SumResponse], error) {
	if p.checkMetadata {
		if err := expectMetadata(stream.RequestHeader(), "header", clientHeader, headerValue); err != nil {
			return nil, err
		}
	}
	if stream.Peer().Addr == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("no peer address"))
	}
	if stream.Peer().Protocol == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("no peer protocol"))
	}
	var sum int64
	for stream.Receive() {
		sum += stream.Msg().GetNumber()
	}
	if stream.Err() != nil {
		return nil, stream.Err()
	}
	response := connect.NewResponse(&pingv1.SumResponse{Sum: sum})
	response.Header().Set(handlerHeader, headerValue)
	response.Trailer().Set(handlerTrailer, trailerValue)
	return response, nil
}

func (p pingServer) CountUp(
	ctx context.Context,
	request *connect.Request[pingv1.CountUpRequest],
	stream *connect.ServerStream[pingv1.CountUpResponse],
) error {
	if err := expectClientHeader(p.checkMetadata, request); err != nil {
		return err
	}
	if request.Peer().Addr == "" {
		return connect.NewError(connect.CodeInternal, errors.New("no peer address"))
	}
	if request.Peer().Protocol == "" {
		return connect.NewError(connect.CodeInternal, errors.New("no peer protocol"))
	}
	if request.Msg.GetNumber() <= 0 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"number must be positive: got %v",
			request.Msg.GetNumber(),
		))
	}
	stream.ResponseHeader().Set(handlerHeader, headerValue)
	stream.ResponseTrailer().Set(handlerTrailer, trailerValue)
	for i := int64(1); i <= request.Msg.GetNumber(); i++ {
		if err := stream.Send(&pingv1.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
	return nil
}

func (p pingServer) CumSum(
	ctx context.Context,
	stream *connect.BidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse],
) error {
	var sum int64
	if p.checkMetadata {
		if err := expectMetadata(stream.RequestHeader(), "header", clientHeader, headerValue); err != nil {
			return err
		}
	}
	if stream.Peer().Addr == "" {
		return connect.NewError(connect.CodeInternal, errors.New("no peer address"))
	}
	if stream.Peer().Protocol == "" {
		return connect.NewError(connect.CodeInternal, errors.New("no peer address"))
	}
	stream.ResponseHeader().Set(handlerHeader, headerValue)
	stream.ResponseTrailer().Set(handlerTrailer, trailerValue)
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		sum += msg.GetNumber()
		if err := stream.Send(&pingv1.CumSumResponse{Sum: sum}); err != nil {
			return err
		}
	}
}

type deflateReader struct {
	r io.ReadCloser
}

func newDeflateReader(r io.Reader) *deflateReader {
	return &deflateReader{r: flate.NewReader(r)}
}

func (d *deflateReader) Read(p []byte) (int, error) {
	return d.r.Read(p)
}

func (d *deflateReader) Close() error {
	return d.r.Close()
}

func (d *deflateReader) Reset(reader io.Reader) error {
	if resetter, ok := d.r.(flate.Resetter); ok {
		return resetter.Reset(reader, nil)
	}
	return fmt.Errorf("flate reader should implement flate.Resetter")
}

var _ connect.Decompressor = (*deflateReader)(nil)

type trimTrailerWriter struct {
	w http.ResponseWriter
}

func (l *trimTrailerWriter) Header() http.Header {
	return l.w.Header()
}

// Write writes b to underlying writer and counts written size.
func (l *trimTrailerWriter) Write(b []byte) (int, error) {
	l.removeTrailers()
	return l.w.Write(b)
}

// WriteHeader writes s to underlying writer and retains the status.
func (l *trimTrailerWriter) WriteHeader(s int) {
	l.removeTrailers()
	l.w.WriteHeader(s)
}

// Flush implements http.Flusher.
func (l *trimTrailerWriter) Flush() {
	l.removeTrailers()
	if f, ok := l.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (l *trimTrailerWriter) removeTrailers() {
	for _, v := range l.w.Header().Values("Trailer") {
		l.w.Header().Del(v)
	}
	l.w.Header().Del("Trailer")
	for k := range l.w.Header() {
		if strings.HasPrefix(k, http.TrailerPrefix) {
			l.w.Header().Del(k)
		}
	}
}

func newHTTPMiddlewareError() *connect.Error {
	err := connect.NewError(connect.CodeResourceExhausted, errors.New("error from HTTP middleware"))
	err.Meta().Set("Middleware-Foo", "bar")
	return err
}

type failDecompressor struct {
	connect.Decompressor
}

type failCompressor struct{}

func (failCompressor) Write([]byte) (int, error) {
	return 0, errors.New("failCompressor")
}

func (failCompressor) Close() error {
	return errors.New("failCompressor")
}

func (failCompressor) Reset(io.Writer) {}
