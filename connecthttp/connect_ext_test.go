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

package connecthttp_test

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	rand "math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/assert"
	"connectrpc.com/connect/v2/internal/gen/connect/import/v1/importv1connect"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/memhttp"
	"connectrpc.com/connect/v2/internal/memhttp/memhttptest"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const errorMessage = "oh no"

// The ping server implementation used in the tests returns errors if the
// client doesn't set a header, and the server sets headers and trailers on the
// response.
const (
	clientHeader                = "Connect-Client-Header"
	handlerHeader               = "Connect-Handler-Header"
	handlerTrailer              = "Connect-Handler-Trailer"
	clientMiddlewareErrorHeader = "Connect-Trigger-HTTP-Error"
)

var (
	expectedHeaderValues = []string{"foo", "bar"} //nolint:gochecknoglobals
)

func TestCallInfo(t *testing.T) {
	t.Parallel()
	t.Run("simple_api", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		srv := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
		connecthttp.Mount(mux, srv)

		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		t.Run("unary", func(t *testing.T) {
			t.Parallel()
			testUnary(t, client)
		})
		t.Run("unary_no_callinfo", func(t *testing.T) {
			t.Parallel()
			num := int64(42)
			expect := &pingv1.PingResponse{Number: num}
			response, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: num})
			assert.Equal(t, response, expect)
			assert.Nil(t, err)
		})
		t.Run("unary_generics_server", func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			srv := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
			connecthttp.Mount(mux, srv)

			server := memhttptest.NewServer(t, mux)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
			testUnary(t, client)
		})
		t.Run("server_stream", func(t *testing.T) {
			t.Parallel()
			testServerStream(t, client)
		})
		t.Run("server_stream_no_callinfo", func(t *testing.T) {
			t.Parallel()
			val := 3
			stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{
				Number: int64(val),
			})
			assert.Nil(t, err)
			// Receive expected messages
			for idx := range val {
				expected := int64(idx + 1)
				msg, err := stream.Receive()
				assert.Nil(t, err)
				assert.NotNil(t, msg)
				assert.Equal(t, msg.GetNumber(), expected)
			}
			_, err = stream.Receive()
			assert.True(t, errors.Is(err, io.EOF))
			assert.Nil(t, stream.Close())
		})
		t.Run("server_stream_generics_server", func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			srv := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
			connecthttp.Mount(mux, srv)

			server := memhttptest.NewServer(t, mux)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
			testServerStream(t, client)
		})
		t.Run("client_stream", func(t *testing.T) {
			t.Parallel()
			testClientStream(t, client)
		})
		t.Run("client_stream_generics_server", func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			srv := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
			connecthttp.Mount(mux, srv)

			server := memhttptest.NewServer(t, mux)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
			testClientStream(t, client)
		})
		t.Run("bidi_stream", func(t *testing.T) {
			t.Parallel()
			testBidiStream(t, client, true)
		})
		t.Run("bidi_stream_generics_server", func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			srv := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
			connecthttp.Mount(mux, srv)

			server := memhttptest.NewServer(t, mux)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
			testBidiStream(t, client, true)
		})
	})
	t.Run("generics_api", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		srv := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
		connecthttp.Mount(mux, srv)

		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		t.Run("unary", func(t *testing.T) {
			t.Parallel()
			testUnary(t, client)
		})
		t.Run("unary_simple_server", func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			srv := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
			connecthttp.Mount(mux, srv)

			server := memhttptest.NewServer(t, mux)
			genericsClient := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
			testUnary(t, genericsClient)
		})
		t.Run("server_stream", func(t *testing.T) {
			t.Parallel()
			testServerStream(t, client)
		})
		t.Run("server_stream_simple_server", func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			srv := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
			connecthttp.Mount(mux, srv)

			server := memhttptest.NewServer(t, mux)
			genericsClient := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
			testServerStream(t, genericsClient)
		})
		t.Run("client_stream", func(t *testing.T) {
			t.Parallel()
			testClientStream(t, client)
		})
		t.Run("client_stream_simple_server", func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			srv := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
			connecthttp.Mount(mux, srv)

			server := memhttptest.NewServer(t, mux)
			genericsClient := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
			testClientStream(t, genericsClient)
		})
		t.Run("bidi_stream", func(t *testing.T) {
			t.Parallel()
			testBidiStream(t, client, true)
		})
		t.Run("bidi_stream_simple_server", func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			srv := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
			connecthttp.Mount(mux, srv)

			server := memhttptest.NewServer(t, mux)
			genericsClient := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
			testBidiStream(t, genericsClient, true)
		})
	})
}

func TestServer(t *testing.T) {
	t.Parallel()
	testPing := func(t *testing.T, client pingv1connect.PingServiceClient) { //nolint:thelper
		t.Run("ping", func(t *testing.T) {
			t.Parallel()
			testUnary(t, client)
		})
		t.Run("zero_ping", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			request := &pingv1.PingRequest{}
			for _, el := range expectedHeaderValues {
				callInfo.RequestHeader().Add(clientHeader, el)
			}
			response, err := client.Ping(ctx, request)
			assert.Nil(t, err)
			var expect pingv1.PingResponse
			assert.Equal(t, response, &expect)
			assertResponseHeadersAndTrailers(t, callInfo)
		})
		t.Run("large_ping", func(t *testing.T) {
			t.Parallel()
			// Using a large payload splits the request and response over multiple
			// packets, ensuring that we're managing HTTP readers and writers
			// correctly.
			if testing.Short() {
				t.Skipf("skipping %s test in short mode", t.Name())
			}
			ctx, callInfo := connect.NewClientContext(t.Context())
			hellos := strings.Repeat("hello", 1024*1024) // ~5mb
			request := &pingv1.PingRequest{Text: hellos}
			for _, el := range expectedHeaderValues {
				callInfo.RequestHeader().Add(clientHeader, el)
			}
			response, err := client.Ping(ctx, request)
			assert.Nil(t, err)
			assert.Equal(t, response.GetText(), hellos)
			assertResponseHeadersAndTrailers(t, callInfo)
		})
		t.Run("ping_error", func(t *testing.T) {
			t.Parallel()
			_, err := client.Ping(
				t.Context(),
				&pingv1.PingRequest{},
			)
			assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument)
		})
		t.Run("ping_timeout", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			defer cancel()
			ctx, callInfo := connect.NewClientContext(ctx)
			request := &pingv1.PingRequest{}
			callInfo.RequestHeader().Set(clientHeader, "foo")
			_, err := client.Ping(ctx, request)
			assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded)
		})
	}
	testSum := func(t *testing.T, client pingv1connect.PingServiceClient) { //nolint:thelper
		t.Run("sum", func(t *testing.T) {
			t.Parallel()
			testClientStream(t, client)
		})
		t.Run("sum_error", func(t *testing.T) {
			t.Parallel()
			stream, err := client.Sum(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(&pingv1.SumRequest{Number: 1}); err != nil {
				assert.ErrorIs(t, err, io.EOF)
				assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
			}
			_, err = stream.CloseAndReceive()
			assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument)
		})
		t.Run("sum_close_and_receive_without_send", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			stream, err := client.Sum(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, el := range expectedHeaderValues {
				callInfo.RequestHeader().Add(clientHeader, el)
			}
			got, err := stream.CloseAndReceive()
			assert.Nil(t, err)
			assert.Equal(t, got, &pingv1.SumResponse{}) // receive header only stream
			assertResponseHeadersAndTrailers(t, callInfo)
		})
	}
	testCountUp := func(t *testing.T, client pingv1connect.PingServiceClient) { //nolint:thelper
		t.Run("count_up", func(t *testing.T) {
			t.Parallel()
			testServerStream(t, client)
		})
		t.Run("count_up_error", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			stream, err := client.CountUp(
				ctx,
				&pingv1.CountUpRequest{Number: 1},
			)
			assert.Nil(t, err)
			for {
				_, err = stream.Receive()
				if err != nil {
					break
				}
				t.Fatalf("expected error, shouldn't receive any messages")
			}
			assert.Equal(
				t,
				connect.CodeOf(err),
				connect.CodeInvalidArgument,
			)
			assert.Nil(t, stream.Close())
		})
		t.Run("count_up_timeout", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			_, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 1})
			assert.NotNil(t, err)
			assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded)
		})
		t.Run("count_up_cancel_after_first_response", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			ctx, callInfo := connect.NewClientContext(ctx)
			request := &pingv1.CountUpRequest{Number: 5}
			callInfo.RequestHeader().Add(clientHeader, "foo")
			callInfo.RequestHeader().Add(clientHeader, "bar")
			stream, err := client.CountUp(ctx, request)
			assert.Nil(t, err)
			_, err = stream.Receive()
			assert.Nil(t, err)
			cancel()
			_, err = stream.Receive()
			assert.NotNil(t, err)
			assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
			assert.Nil(t, stream.Close())
		})
	}
	testCumSum := func(t *testing.T, client pingv1connect.PingServiceClient, expectSuccess bool) { //nolint:thelper
		t.Run("cumsum", func(t *testing.T) {
			t.Parallel()
			testBidiStream(t, client, expectSuccess)
		})
		t.Run("cumsum_error", func(t *testing.T) {
			t.Parallel()
			stream, err := client.CumSum(t.Context())
			if err != nil {
				t.Fatal(err)
			}
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
			_, err = stream.Receive()
			assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument)
			var cerr *connect.Error
			assert.True(t, errors.As(err, &cerr))
			assert.True(t, cerr.IsRemote())
		})
		t.Run("cumsum_empty_stream", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			stream, err := client.CumSum(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, el := range expectedHeaderValues {
				callInfo.RequestHeader().Add(clientHeader, el)
			}
			if !expectSuccess { // server doesn't support HTTP/2
				failNoHTTP2(t, stream)
				return
			}
			// Deliberately closing with calling Send to test the behavior of Receive.
			// This test case is based on the grpc interop tests.
			assert.Nil(t, stream.CloseSend())
			response, err := stream.Receive()
			assert.Nil(t, response)
			assert.True(t, errors.Is(err, io.EOF))
			var cerr *connect.Error
			assert.True(t, errors.As(err, &cerr))
			assert.False(t, cerr.IsRemote())
			assert.Nil(t, stream.Close()) // clean-up the stream
		})
		t.Run("cumsum_cancel_after_first_response", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			ctx, callInfo := connect.NewClientContext(ctx)
			stream, err := client.CumSum(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, el := range expectedHeaderValues {
				callInfo.RequestHeader().Add(clientHeader, el)
			}
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
			var cerr *connect.Error
			assert.True(t, errors.As(err, &cerr))
			assert.False(t, cerr.IsRemote())
			assert.Nil(t, stream.Close())
		})
		t.Run("cumsum_cancel_before_send", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			ctx, callInfo := connect.NewClientContext(ctx)
			stream, err := client.CumSum(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !expectSuccess { // server doesn't support HTTP/2
				failNoHTTP2(t, stream)
				cancel()
				return
			}
			for _, el := range expectedHeaderValues {
				callInfo.RequestHeader().Add(clientHeader, el)
			}
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 8}))
			cancel()
			// On a subsequent send, ensure that we are still catching context
			// cancellations.
			err = stream.Send(&pingv1.CumSumRequest{Number: 19})
			assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled, assert.Sprintf("%v", err))
			var cerr *connect.Error
			assert.True(t, errors.As(err, &cerr))
			assert.False(t, cerr.IsRemote())
			assert.Nil(t, stream.CloseSend())
			assert.Nil(t, stream.Close())
		})
	}
	testErrors := func(t *testing.T, client pingv1connect.PingServiceClient) { //nolint:thelper
		assertIsHTTPMiddlewareError := func(tb testing.TB, ctx context.Context, err error) {
			tb.Helper()
			assert.NotNil(tb, err)
			var connectErr *connect.Error
			assert.True(tb, errors.As(err, &connectErr))
			expect := connect.NewError(connect.CodeResourceExhausted, "error from HTTP middleware")
			assert.Equal(tb, connectErr.Code(), expect.Code())
			assert.Equal(tb, connectErr.Message(), expect.Message())
			callInfo, _ := connect.CallInfoForClientContext(ctx)
			assert.NotNil(t, callInfo)
			got := callInfo.ResponseHeader().Get("Middleware-Foo")
			assert.Equal(t, got, "bar")
			assert.Equal(tb, len(connectErr.Details()), len(expect.Details()))
		}
		t.Run("errors", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			request := &pingv1.FailRequest{
				Code: int32(connect.CodeResourceExhausted),
			}
			for _, el := range expectedHeaderValues {
				callInfo.RequestHeader().Add(clientHeader, el)
			}

			response, err := client.Fail(ctx, request)
			assert.Nil(t, response)
			assert.NotNil(t, err)
			var connectErr *connect.Error
			ok := errors.As(err, &connectErr)
			assert.True(t, ok, assert.Sprintf("conversion to *connect.Error"))
			var cerr *connect.Error
			assert.True(t, errors.As(err, &cerr))
			assert.True(t, cerr.IsRemote())
			assert.Equal(t, connectErr.Code(), connect.CodeResourceExhausted)
			assert.Equal(t, connectErr.Error(), "resource_exhausted: "+errorMessage)
			assert.Zero(t, connectErr.Details())
			assertErrorResponseMetadata(t, callInfo)
		})
		t.Run("middleware_errors_unary", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			request := &pingv1.PingRequest{}
			for _, el := range expectedHeaderValues {
				callInfo.RequestHeader().Set(clientMiddlewareErrorHeader, el)
			}
			_, err := client.Ping(ctx, request)
			assertIsHTTPMiddlewareError(t, ctx, err)
		})
		t.Run("middleware_errors_streaming", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			request := &pingv1.CountUpRequest{Number: 10}
			for _, el := range expectedHeaderValues {
				callInfo.RequestHeader().Set(clientMiddlewareErrorHeader, el)
			}
			stream, err := client.CountUp(ctx, request)
			assert.Nil(t, err)
			_, err = stream.Receive()
			assert.NotNil(t, err)
			assertIsHTTPMiddlewareError(t, ctx, err)
		})
		_ = assertIsHTTPMiddlewareError
	}
	testMatrix := func(t *testing.T, client *http.Client, url string, bidi bool) { //nolint:thelper
		run := func(t *testing.T, opts ...connecthttp.Option) {
			t.Helper()
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(client, url, opts...)))
			testPing(t, client)
			testSum(t, client)
			testCountUp(t, client)
			testCumSum(t, client, bidi)
			testErrors(t, client)
		}
		t.Run("connect", func(t *testing.T) {
			t.Parallel()
			t.Run("proto", func(t *testing.T) {
				t.Parallel()
				run(t)
			})
			t.Run("proto_gzip", func(t *testing.T) {
				t.Parallel()
				run(t, connecthttp.WithSendCompression("gzip"))
			})
			t.Run("json_gzip", func(t *testing.T) {
				t.Parallel()
				run(
					t,
					connecthttp.WithSendCodec(connect.CodecNameJSON),
					connecthttp.WithSendCompression("gzip"),
				)
			})
			t.Run("json_get", func(t *testing.T) {
				t.Parallel()
				run(
					t,
					connecthttp.WithSendCodec(connect.CodecNameJSON),
					connecthttp.WithHTTPGet(),
					connecthttp.WithHTTPGetMaxURLSize(1024, true),
				)
			})
		})
		t.Run("grpc", func(t *testing.T) {
			t.Parallel()
			t.Run("proto", func(t *testing.T) {
				t.Parallel()
				run(t, connecthttp.WithGRPC())
			})
			t.Run("proto_gzip", func(t *testing.T) {
				t.Parallel()
				run(t, connecthttp.WithGRPC(), connecthttp.WithSendCompression("gzip"))
			})
			t.Run("json_gzip", func(t *testing.T) {
				t.Parallel()
				run(
					t,
					connecthttp.WithGRPC(),
					connecthttp.WithSendCodec(connect.CodecNameJSON),
					connecthttp.WithSendCompression("gzip"),
				)
			})
		})
		t.Run("grpcweb", func(t *testing.T) {
			t.Parallel()
			t.Run("proto", func(t *testing.T) {
				t.Parallel()
				run(t, connecthttp.WithGRPCWeb())
			})
			t.Run("proto_gzip", func(t *testing.T) {
				t.Parallel()
				run(t, connecthttp.WithGRPCWeb(), connecthttp.WithSendCompression("gzip"))
			})
			t.Run("json_gzip", func(t *testing.T) {
				t.Parallel()
				run(
					t,
					connecthttp.WithGRPCWeb(),
					connecthttp.WithSendCodec(connect.CodecNameJSON),
					connecthttp.WithSendCompression("gzip"),
				)
			})
		})
	}

	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{checkMetadata: true})
	innerMux := http.NewServeMux()
	connecthttp.Mount(innerMux, srv)
	errorWriter := connecthttp.NewErrorWriter()
	httpMiddlewareErr := connect.NewError(connect.CodeResourceExhausted, "error from HTTP middleware")

	// Add net/http middleware to the ping service to evaluate HTTP state.
	mux.Handle("/", http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		// Exercise ErrorWriter for HTTP middleware errors.
		if request.Header.Get(clientMiddlewareErrorHeader) != "" {
			defer request.Body.Close()
			if _, err := io.Copy(io.Discard, request.Body); err != nil {
				t.Errorf("drain request body: %v", err)
			}
			if !errorWriter.IsSupported(request) {
				t.Errorf("ErrorWriter doesn't support Content-Type %q", request.Header.Get("Content-Type"))
			}
			response.Header().Set("Middleware-Foo", "bar")
			if err := errorWriter.Write(response, request, httpMiddlewareErr); err != nil {
				t.Errorf("send RPC error from HTTP middleware: %v", err)
			}
			return
		}
		// Check Content-Length is set correctly.
		switch request.URL.Path {
		case pingv1connect.PingServicePingProcedure,
			pingv1connect.PingServiceFailProcedure,
			pingv1connect.PingServiceCountUpProcedure:
			// Unary requests set Content-Length to the length of the request body.
			if request.ContentLength < 0 {
				t.Errorf("%s: expected Content-Length >= 0, got %d", request.URL.Path, request.ContentLength)
			}
		case pingv1connect.PingServiceSumProcedure,
			pingv1connect.PingServiceCumSumProcedure:
			// Streaming requests set Content-Length to -1 or 0 on empty requests.
			if request.ContentLength > 0 {
				t.Errorf("%s: expected Content-Length -1 or 0, got %d", request.URL.Path, request.ContentLength)
			}
		default:
			t.Errorf("unexpected path %q", request.URL.Path)
		}
		innerMux.ServeHTTP(response, request)
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
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)
	var done, start sync.WaitGroup
	start.Add(1)
	for range runtime.GOMAXPROCS(0) * 8 {
		done.Go(func() {
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
			var total int64
			sum, err := client.CumSum(t.Context())
			if err != nil {
				t.Errorf("failed to open stream: %v", err)
				return
			}
			start.Wait()
			for range 100 {
				num := rand.Int64N(1000)
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
			if err := sum.CloseSend(); err != nil {
				t.Errorf("failed to close request: %v", err)
			}
			if err := sum.Close(); err != nil {
				t.Errorf("failed to close response: %v", err)
			}
		})
	}
	start.Done()
	done.Wait()
}

func TestErrorHeaderPropagation(t *testing.T) {
	t.Parallel()

	newError := func(ctx context.Context, testname string, isWire bool) *connect.Error {
		err := connect.NewError(connect.CodeInvalidArgument, testname)
		if isWire {
			err = err.WithRemote()
		}
		msgDetail, detailErr := connectproto.NewErrorDetail(&wrapperspb.StringValue{Value: "server details"})
		if detailErr != nil {
			return connect.NewError(connect.CodeInternal, detailErr.Error())
		}
		err = err.WithDetail(msgDetail)
		callInfo, _ := connect.CallInfoForServerContext(ctx)
		callInfo.ResponseHeader().Set("Content-Length", "1337")
		callInfo.ResponseHeader().Set("Content-Type", "application/xml")
		callInfo.ResponseHeader().Set("Accept-Encoding", "bogus")
		callInfo.ResponseHeader().Set("Date", "Thu, 01 Jan 1970 00:00:00 GMT")
		callInfo.ResponseHeader().Set("Grpc-Status", "0")
		// Set custom headers.
		callInfo.ResponseHeader().Set("X-Test", testname)
		callInfo.ResponseHeader().SetValues("x-test-case", []string{testname})
		return err
	}
	pingServer := &pluggablePingServer{
		ping: func(ctx context.Context, request *pingv1.PingRequest) (*pingv1.PingResponse, error) {
			callInfo, _ := connect.CallInfoForServerContext(ctx)
			xTest := callInfo.RequestHeader().Get("X-Test")
			xTestWire := callInfo.RequestHeader().Get("X-Test-Is-Wire")
			return nil, newError(ctx, xTest, xTestWire == "true")
		},
		cumSum: func(ctx context.Context, request pingv1connect.PingServiceCumSumServerStream) error {
			callInfo, _ := connect.CallInfoForServerContext(ctx)
			xTest := callInfo.RequestHeader().Get("X-Test")
			xTestWire := callInfo.RequestHeader().Get("X-Test-Is-Wire")
			return newError(ctx, xTest, xTestWire == "true")
		},
	}
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer)
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)

	assertHeaders := func(t *testing.T, ctx context.Context) {
		t.Helper()
		callInfo, _ := connect.CallInfoForClientContext(ctx)
		assert.NotEqual(t, metaValues(callInfo, "Content-Length"), []string{"1337"})
		assert.NotEqual(t, metaValues(callInfo, "Accept-Encoding"), []string{"bogus"})
		assert.NotEqual(t, metaValues(callInfo, "Content-Type"), []string{"application/xml"})
		assert.NotEqual(t, metaValues(callInfo, "Date"), []string{"Thu, 01 Jan 1970 00:00:00 GMT"})
		assert.Equal(t, metaValues(callInfo, "x-test-case"), []string{t.Name()})
		assert.Equal(t, metaValues(callInfo, "X-Test"), []string{t.Name()})
	}
	assertError := func(t *testing.T, ctx context.Context, err error) {
		t.Helper()
		var connectErr *connect.Error
		if !assert.True(t, errors.As(err, &connectErr)) {
			return
		}
		assert.Equal(t, connectErr.Code(), connect.CodeInvalidArgument)
		assert.Equal(t, connectErr.Message(), t.Name())
		details := connectErr.Details()
		if assert.Equal(t, len(details), 1) {
			detailMsg, err := connectproto.UnmarshalErrorDetail(details[0])
			if !assert.Nil(t, err) {
				return
			}
			serverDetails, ok := detailMsg.(*wrapperspb.StringValue)
			if !assert.True(t, ok) {
				return
			}
			assert.Equal(t, serverDetails.Value, "server details")
		}
		assertHeaders(t, ctx)
	}
	// A handler-returned remote error is scrubbed to CodeInternal with no
	// message or details; headers still propagate.
	assertScrubbedError := func(t *testing.T, ctx context.Context, err error) {
		t.Helper()
		var connectErr *connect.Error
		if !assert.True(t, errors.As(err, &connectErr)) {
			return
		}
		assert.Equal(t, connectErr.Code(), connect.CodeInternal)
		assert.Equal(t, connectErr.Message(), "")
		assert.Equal(t, len(connectErr.Details()), 0)
		assertHeaders(t, ctx)
	}
	testServices := func(t *testing.T, client pingv1connect.PingServiceClient) {
		t.Helper()
		t.Run("unary", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			request := &pingv1.PingRequest{}
			callInfo.RequestHeader().Set("X-Test", t.Name())
			_, err := client.Ping(ctx, request)
			if !assert.NotNil(t, err) {
				return
			}
			assertError(t, ctx, err)
			t.Run("wire", func(t *testing.T) {
				t.Parallel()
				ctx, callInfo := connect.NewClientContext(t.Context())
				request := &pingv1.PingRequest{}
				callInfo.RequestHeader().Set("X-Test", t.Name())
				callInfo.RequestHeader().Set("X-Test-Is-Wire", "true")
				_, err := client.Ping(ctx, request)
				if !assert.NotNil(t, err) {
					return
				}
				assertScrubbedError(t, ctx, err)
			})
		})
		t.Run("bidi", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			stream, err := client.CumSum(ctx)
			if err != nil {
				t.Fatal(err)
			}
			callInfo.RequestHeader().Set("X-Test", t.Name())
			if err := stream.SendHeaders(); err != nil {
				t.Fatal(err)
			}
			_, err = stream.Receive()
			if !assert.NotNil(t, err) {
				return
			}
			assertError(t, ctx, err)
			t.Run("wire", func(t *testing.T) {
				ctx, callInfo := connect.NewClientContext(t.Context())
				t.Parallel()
				stream, err := client.CumSum(ctx)
				if err != nil {
					t.Fatal(err)
				}
				callInfo.RequestHeader().Set("X-Test", t.Name())
				callInfo.RequestHeader().Set("X-Test-Is-Wire", "true")
				if err := stream.SendHeaders(); err != nil {
					t.Fatal(err)
				}
				_, err = stream.Receive()
				if !assert.NotNil(t, err) {
					return
				}
				assertScrubbedError(t, ctx, err)
			})
		})
	}
	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		testServices(t, client)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPC())))
		testServices(t, client)
	})
	t.Run("grpc-web", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPCWeb())))
		testServices(t, client)
	})
}

// TestCallInfoDetails verifies transports populate CallInfo's codec,
// encoding, and message-stats fields on both sides of a call.
func TestCallInfoDetails(t *testing.T) {
	t.Parallel()
	pingServer := &pluggablePingServer{
		ping: func(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
			info, _ := connect.CallInfoForServerContext(ctx)
			if info.Codec == "" {
				return nil, connect.NewError(connect.CodeInternal, "codec not set")
			}
			if want := info.RequestHeader().Get("Expect-Request-Encoding"); info.RequestEncoding != want {
				return nil, connect.Errorf(connect.CodeInternal, "RequestEncoding = %q, want %q", info.RequestEncoding, want)
			}
			if info.ResponseEncoding == "" {
				return nil, connect.NewError(connect.CodeInternal, "response encoding not set")
			}
			if info.ReceiveStats.Size == 0 {
				return nil, connect.NewError(connect.CodeInternal, "ReceiveStats.Size not set")
			}
			if wantCompressed := info.RequestEncoding != connect.CompressionNameIdentity; wantCompressed != (info.ReceiveStats.CompressedSize > 0) {
				return nil, connect.Errorf(connect.CodeInternal, "ReceiveStats.CompressedSize = %d with encoding %q", info.ReceiveStats.CompressedSize, info.RequestEncoding)
			}
			return &pingv1.PingResponse{Number: req.GetNumber(), Text: req.GetText()}, nil
		},
		cumSum: func(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
			var sum int64
			for {
				req, err := stream.Receive()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				sum += req.GetNumber()
				if err := stream.Send(&pingv1.CumSumResponse{Sum: sum}); err != nil {
					return err
				}
			}
		},
	}
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer)
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)

	assertUnary := func(t *testing.T, wantCodec, wantRequestEncoding string, options ...connecthttp.Option) {
		t.Helper()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), options...)))
		ctx, info := connect.NewClientContext(t.Context())
		info.RequestHeader().Set("Expect-Request-Encoding", wantRequestEncoding)
		_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 42, Text: strings.Repeat("connect", 32)})
		if err != nil {
			t.Fatalf("Ping: %v", err)
		}
		assert.Equal(t, info.Codec, wantCodec)
		assert.Equal(t, info.RequestEncoding, wantRequestEncoding)
		assert.Equal(t, info.ResponseEncoding, connect.CompressionNameGzip)
		assert.True(t, info.SendStats.Size > 0)
		assert.Equal(t, info.SendStats.CompressedSize > 0, wantRequestEncoding == connect.CompressionNameGzip)
		assert.True(t, info.ReceiveStats.Size > 0)
		assert.True(t, info.ReceiveStats.CompressedSize > 0)
	}
	protocols := map[string][]connecthttp.Option{
		"connect":  nil,
		"grpc":     {connecthttp.WithGRPC()},
		"grpc_web": {connecthttp.WithGRPCWeb()},
	}
	for name, protocolOpts := range protocols {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertUnary(t, connect.CodecNameProto, connect.CompressionNameIdentity, protocolOpts...)
		})
		t.Run(name+"_gzip", func(t *testing.T) {
			t.Parallel()
			assertUnary(t, connect.CodecNameProto, connect.CompressionNameGzip, append(protocolOpts, connecthttp.WithSendGzip())...)
		})
	}
	t.Run("connect_json", func(t *testing.T) {
		t.Parallel()
		assertUnary(t, connect.CodecNameJSON, connect.CompressionNameIdentity, connecthttp.WithProtoJSON())
	})
	t.Run("connect_streaming", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		ctx, info := connect.NewClientContext(t.Context())
		stream, err := client.CumSum(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if err := stream.Send(&pingv1.CumSumRequest{Number: 42}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if _, err := stream.Receive(); err != nil {
			t.Fatalf("Receive: %v", err)
		}
		assert.Equal(t, info.Codec, connect.CodecNameProto)
		assert.Equal(t, info.RequestEncoding, connect.CompressionNameIdentity)
		assert.Equal(t, info.ResponseEncoding, connect.CompressionNameGzip)
		assert.True(t, info.SendStats.Size > 0)
		assert.Equal(t, info.SendStats.CompressedSize, 0)
		assert.True(t, info.ReceiveStats.Size > 0)
		assert.True(t, info.ReceiveStats.CompressedSize > 0)
	})
}

// TestMountUnknownMethod verifies RPC requests for unknown methods of a
// registered service fail with CodeUnimplemented instead of falling through
// to sibling mux routes.
func TestMountUnknownMethod(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	// A sibling catch-all route: unknown-method requests must not reach it.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	server := memhttptest.NewServer(t, mux)

	unknownProcedure := "/connect.ping.v1.PingService/DoesNotExist"
	unknownSpec := connect.Spec{
		StreamType: connect.StreamTypeUnary,
		Procedure:  unknownProcedure,
	}
	t.Run("raw_http", func(t *testing.T) {
		t.Parallel()
		request, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			server.URL()+unknownProcedure,
			strings.NewReader(""),
		)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/proto")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		assert.Equal(t, response.StatusCode, http.StatusNotImplemented)
		var wireErr struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(response.Body).Decode(&wireErr); err != nil {
			t.Fatal(err)
		}
		assert.Equal(t, wireErr.Code, "unimplemented")
	})
	t.Run("connect_client", func(t *testing.T) {
		t.Parallel()
		client := connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL()))
		err := client.CallUnary(t.Context(), unknownSpec, &pingv1.PingRequest{}, &pingv1.PingResponse{})
		assert.Equal(t, connect.CodeOf(err), connect.CodeUnimplemented)
	})
	t.Run("grpc_client", func(t *testing.T) {
		t.Parallel()
		client := connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPC()))
		err := client.CallUnary(t.Context(), unknownSpec, &pingv1.PingRequest{}, &pingv1.PingResponse{})
		assert.Equal(t, connect.CodeOf(err), connect.CodeUnimplemented)
	})
	t.Run("grpc_web_client", func(t *testing.T) {
		t.Parallel()
		client := connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPCWeb()))
		err := client.CallUnary(t.Context(), unknownSpec, &pingv1.PingRequest{}, &pingv1.PingResponse{})
		assert.Equal(t, connect.CodeOf(err), connect.CodeUnimplemented)
	})
	t.Run("registered_method", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 42})
		assert.Nil(t, err)
	})
	t.Run("unknown_service", func(t *testing.T) {
		t.Parallel()
		request, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			server.URL()+"/some.other.v1.Service/Method",
			strings.NewReader(""),
		)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/proto")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		assert.Equal(t, response.StatusCode, http.StatusTeapot)
	})
}

// TestMountUnknownHandler verifies a SetUnknownHandler fallback answers
// unknown-method RPCs dispatched by Mount's catch-all.
func TestMountUnknownHandler(t *testing.T) {
	t.Parallel()
	const fallbackText = "fallback"
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	// Set after Mount: the fallback is read at call time.
	srv.SetUnknownHandler(func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		info, _ := connect.CallInfoForServerContext(ctx)
		info.ResponseHeader().Set("Fallback-Procedure", spec.Procedure)
		var req pingv1.PingRequest
		if err := stream.Receive(&req); err != nil {
			return err
		}
		return stream.Send(&pingv1.PingResponse{Number: req.GetNumber(), Text: fallbackText})
	})
	server := memhttptest.NewServer(t, mux)

	unknownProcedure := "/connect.ping.v1.PingService/DoesNotExist"
	unknownSpec := connect.Spec{
		StreamType: connect.StreamTypeUnary,
		Procedure:  unknownProcedure,
	}
	testUnaryFallback := func(t *testing.T, options ...connecthttp.Option) {
		t.Helper()
		client := connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), options...))
		ctx, callInfo := connect.NewClientContext(t.Context())
		var res pingv1.PingResponse
		if err := client.CallUnary(ctx, unknownSpec, &pingv1.PingRequest{Number: 42}, &res); err != nil {
			t.Fatalf("CallUnary: %v", err)
		}
		if res.GetText() != fallbackText || res.GetNumber() != 42 {
			t.Errorf("response = %d %q, want 42 %q", res.GetNumber(), res.GetText(), fallbackText)
		}
		if got := callInfo.ResponseHeader().Get("Fallback-Procedure"); got != unknownProcedure {
			t.Errorf("Fallback-Procedure = %q, want %q", got, unknownProcedure)
		}
	}
	t.Run("connect_client", func(t *testing.T) {
		t.Parallel()
		testUnaryFallback(t)
	})
	t.Run("grpc_client", func(t *testing.T) {
		t.Parallel()
		testUnaryFallback(t, connecthttp.WithGRPC())
	})
	t.Run("grpc_web_client", func(t *testing.T) {
		t.Parallel()
		testUnaryFallback(t, connecthttp.WithGRPCWeb())
	})
	t.Run("connect_streaming_client", func(t *testing.T) {
		t.Parallel()
		client := connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL()))
		stream, err := client.CallClientStream(t.Context(), connect.Spec{
			StreamType: connect.StreamTypeBidi,
			Procedure:  unknownProcedure,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if err := stream.Send(&pingv1.PingRequest{Number: 7}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		var res pingv1.PingResponse
		if err := stream.Receive(&res); err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if res.GetText() != fallbackText || res.GetNumber() != 7 {
			t.Errorf("response = %d %q, want 7 %q", res.GetNumber(), res.GetText(), fallbackText)
		}
		if err := stream.Receive(&res); !errors.Is(err, io.EOF) {
			t.Errorf("second Receive = %v, want io.EOF", err)
		}
	})
	t.Run("registered_method_unaffected", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		res, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 42})
		if err != nil {
			t.Fatalf("Ping: %v", err)
		}
		if res.GetText() == fallbackText {
			t.Error("registered method was served by the fallback")
		}
	})
	assertRawStatus := func(t *testing.T, method, contentType string, wantStatus int) {
		t.Helper()
		request, err := http.NewRequestWithContext(
			t.Context(),
			method,
			server.URL()+unknownProcedure,
			strings.NewReader(""),
		)
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		assert.Equal(t, response.StatusCode, wantStatus)
	}
	t.Run("get_not_dispatched", func(t *testing.T) {
		t.Parallel()
		assertRawStatus(t, http.MethodGet, "", http.StatusNotFound)
	})
	t.Run("non_rpc_post_not_dispatched", func(t *testing.T) {
		t.Parallel()
		assertRawStatus(t, http.MethodPost, "text/plain", http.StatusNotFound)
	})
}

// TestHandlerErrorScrub verifies handler errors that are not *connect.Error
// reach the client as a bare code with no message.
func TestHandlerErrorScrub(t *testing.T) {
	t.Parallel()
	handlerErrs := map[string]error{
		"plain":    errors.New("disk full: /var/data"),
		"wrapped":  fmt.Errorf("query users: %w", errors.New("pq: connection refused")),
		"canceled": fmt.Errorf("copy data: %w", context.Canceled),
		"deadline": fmt.Errorf("copy data: %w", context.DeadlineExceeded),
	}
	wantCodes := map[string]connect.Code{
		"plain":    connect.CodeUnknown,
		"wrapped":  connect.CodeUnknown,
		"canceled": connect.CodeCanceled,
		"deadline": connect.CodeDeadlineExceeded,
	}
	pingServer := &pluggablePingServer{
		ping: func(ctx context.Context, _ *pingv1.PingRequest) (*pingv1.PingResponse, error) {
			info, _ := connect.CallInfoForServerContext(ctx)
			key := info.RequestHeader().Get("X-Error-Case")
			return nil, handlerErrs[key]
		},
		cumSum: func(ctx context.Context, _ pingv1connect.PingServiceCumSumServerStream) error {
			info, _ := connect.CallInfoForServerContext(ctx)
			key := info.RequestHeader().Get("X-Error-Case")
			return handlerErrs[key]
		},
	}
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer)
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)

	assertScrubbed := func(t *testing.T, err error, wantCode connect.Code) {
		t.Helper()
		if !assert.NotNil(t, err) {
			return
		}
		var connectErr *connect.Error
		if !assert.True(t, errors.As(err, &connectErr)) {
			return
		}
		assert.Equal(t, connectErr.Code(), wantCode)
		assert.Equal(t, connectErr.Message(), "")
	}
	testCases := func(t *testing.T, client pingv1connect.PingServiceClient) {
		t.Helper()
		for name := range handlerErrs {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				ctx, callInfo := connect.NewClientContext(t.Context())
				callInfo.RequestHeader().Set("X-Error-Case", name)
				_, err := client.Ping(ctx, &pingv1.PingRequest{})
				assertScrubbed(t, err, wantCodes[name])
			})
			t.Run(name+"_stream", func(t *testing.T) {
				t.Parallel()
				ctx, callInfo := connect.NewClientContext(t.Context())
				callInfo.RequestHeader().Set("X-Error-Case", name)
				stream, err := client.CumSum(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				if err := stream.SendHeaders(); err != nil {
					t.Fatal(err)
				}
				_, err = stream.Receive()
				assertScrubbed(t, err, wantCodes[name])
			})
		}
	}
	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		testCases(t, client)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPC())))
		testCases(t, client)
	})
	t.Run("grpc-web", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPCWeb())))
		testCases(t, client)
	})
}

func TestHeaderBasic(t *testing.T) {
	t.Parallel()
	const (
		key  = "Test-Key"
		cval = "client value"
		hval = "client value"
	)

	pingServer := &pluggablePingServer{
		ping: func(ctx context.Context, request *pingv1.PingRequest) (*pingv1.PingResponse, error) {
			info, _ := connect.CallInfoForServerContext(ctx)
			reqVal := info.RequestHeader().Get(key)
			assert.Equal(t, reqVal, cval)
			info.ResponseHeader().Set(key, hval)
			return &pingv1.PingResponse{}, nil
		},
	}
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer)
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)

	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
	ctx, callInfo := connect.NewClientContext(t.Context())
	callInfo.RequestHeader().Set(key, cval)
	_, err := client.Ping(ctx, &pingv1.PingRequest{})
	assert.Nil(t, err)
	respVal := callInfo.ResponseHeader().Get(key)
	assert.Equal(t, respVal, hval)
}

func TestHeaderHost(t *testing.T) {
	t.Parallel()
	const (
		key  = "Host"
		cval = "buf.build"
	)

	pingServer := &pluggablePingServer{
		ping: func(ctx context.Context, _ *pingv1.PingRequest) (*pingv1.PingResponse, error) {
			info, _ := connect.CallInfoForServerContext(ctx)
			reqVal := info.RequestHeader().Get(key)
			assert.Equal(t, reqVal, cval)
			return &pingv1.PingResponse{}, nil
		},
	}

	newHTTP2Server := func(t *testing.T) *memhttp.Server {
		t.Helper()
		mux := http.NewServeMux()
		srv := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(srv, pingServer)
		connecthttp.Mount(mux, srv)
		server := memhttptest.NewServer(t, mux)
		return server
	}

	callWithHost := func(t *testing.T, client pingv1connect.PingServiceClient) {
		t.Helper()

		ctx, callInfo := connect.NewClientContext(t.Context())
		callInfo.RequestHeader().Set(key, cval)
		_, err := client.Ping(ctx, &pingv1.PingRequest{})
		assert.Nil(t, err)
		assert.Equal(t, callInfo.ResponseHeader().Has(key), false)
	}

	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		callWithHost(t, client)
	})

	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPC())))
		callWithHost(t, client)
	})

	t.Run("grpc-web", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPCWeb())))
		callWithHost(t, client)
	})
}

func TestTimeoutParsing(t *testing.T) {
	t.Parallel()
	const timeout = 10 * time.Minute
	pingServer := &pluggablePingServer{
		ping: func(ctx context.Context, request *pingv1.PingRequest) (*pingv1.PingResponse, error) {
			deadline, ok := ctx.Deadline()
			assert.True(t, ok)
			remaining := time.Until(deadline)
			assert.True(t, remaining > 0)
			assert.True(t, remaining <= timeout)
			return &pingv1.PingResponse{}, nil
		},
	}
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer)
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
	_, err := client.Ping(ctx, &pingv1.PingRequest{})
	assert.Nil(t, err)
}

func TestFailCodec(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	server := memhttptest.NewServer(t, handler)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
		server.URL(),
		connecthttp.WithCodec(failCodec{}))),
	)
	stream, err := client.CumSum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = stream.Send(&pingv1.CumSumRequest{})
	var connectErr *connect.Error
	assert.NotNil(t, err)
	assert.True(t, errors.As(err, &connectErr))
	assert.Equal(t, connectErr.Code(), connect.CodeInternal)
}

func TestContextError(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	server := memhttptest.NewServer(t, handler)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
		server.URL())),
	)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stream, err := client.CumSum(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = stream.Send(&pingv1.CumSumRequest{})
	var connectErr *connect.Error
	assert.NotNil(t, err)
	assert.True(t, errors.As(err, &connectErr))
	assert.Equal(t, connectErr.Code(), connect.CodeCanceled)
	assert.False(t, connectErr.IsRemote())
}

func TestGRPCMarshalStatus(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{
		// Include error details in the response, so that the Status protobuf will be marshaled.
		includeErrorDetails: true,
	})
	connecthttp.Mount(mux, srv)

	server := memhttptest.NewServer(t, mux)
	assertInternalError := func(tb testing.TB, opts ...connecthttp.Option) {
		tb.Helper()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), opts...)))
		request := &pingv1.FailRequest{Code: int32(connect.CodeResourceExhausted)}
		_, err := client.Fail(t.Context(), request)
		tb.Log(err)
		assert.NotNil(t, err, assert.Sprintf("expected an error"))
		var connectErr *connect.Error
		ok := errors.As(err, &connectErr)
		assert.True(t, ok, assert.Sprintf("expected the error to be a connect.Error"))
		assert.Equal(t, connectErr.Code(), connect.CodeResourceExhausted)
		assert.True(t, strings.HasSuffix(connectErr.Message(), errorMessage))
	}

	// Only applies to gRPC protocols, where we're marshaling the Status protobuf
	// message to binary.
	assertInternalError(t, connecthttp.WithGRPC())
	assertInternalError(t, connecthttp.WithGRPCWeb())
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
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{checkMetadata: true})
	connecthttp.Mount(mux, srv)

	server := memhttptest.NewServer(t, trimTrailers(mux))
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPC())))

	assertErrorNoTrailers := func(t *testing.T, err error) {
		t.Helper()
		assert.NotNil(t, err)
		var connectErr *connect.Error
		ok := errors.As(err, &connectErr)
		assert.True(t, ok)
		assert.Equal(t, connectErr.Code(), connect.CodeUnknown, assert.Sprintf("%s", err))
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
		request := &pingv1.PingRequest{Number: 1, Text: "foobar"}
		_, err := client.Ping(t.Context(), request)
		assertErrorNoTrailers(t, err)
	})
	t.Run("sum", func(t *testing.T) {
		t.Parallel()
		stream, err := client.Sum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		err = stream.Send(&pingv1.SumRequest{Number: 1})
		assertNilOrEOF(t, err)
		_, err = stream.CloseAndReceive()
		assertErrorNoTrailers(t, err)
	})
	t.Run("count_up", func(t *testing.T) {
		t.Parallel()
		stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 10})
		assert.Nil(t, err)
		_, err = stream.Receive()
		assertErrorNoTrailers(t, err)
		assert.Nil(t, stream.Close())
	})
	t.Run("cumsum", func(t *testing.T) {
		t.Parallel()
		stream, err := client.CumSum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		assertNilOrEOF(t, stream.Send(&pingv1.CumSumRequest{Number: 10}))
		response, err := stream.Receive()
		assert.Nil(t, response)
		assertErrorNoTrailers(t, err)
		assert.Nil(t, stream.Close())
	})
	t.Run("cumsum_empty_stream", func(t *testing.T) {
		t.Parallel()
		stream, err := client.CumSum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		assert.Nil(t, stream.CloseSend())
		response, err := stream.Receive()
		assert.Nil(t, response)
		assertErrorNoTrailers(t, err)
		assert.Nil(t, stream.Close())
	})
}

func TestUnavailableIfHostInvalid(t *testing.T) {
	t.Parallel()
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient,
		"https://api.invalid/")),
	)
	_, err := client.Ping(
		t.Context(),
		&pingv1.PingRequest{},
	)
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable)
}

func TestBidiRequiresHTTP2(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+proto")
		_, err := io.WriteString(w, "hello world")
		assert.Nil(t, err)
	})
	server := memhttptest.NewServer(t, handler)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: server.TransportHTTP1()},
		server.URL())),
	)
	stream, err := client.CumSum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Stream creates an async request, can error on Send or Receive.
	if err := stream.Send(&pingv1.CumSumRequest{}); err != nil {
		assert.ErrorIs(t, err, io.EOF)
	}
	assert.Nil(t, stream.CloseSend())
	_, err = stream.Receive()
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
			writer.Header().Set("Content-Type", "application/proto")
			assert.Equal(tb, request.Header.Get("Content-Encoding"), expect)
		}))
		server := memhttptest.NewServer(t, mux)
		_, err := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
			server.URL(),
			connecthttp.WithSendCompression("gzip"),
			connecthttp.WithCompressMinBytes(8))),
		).Ping(t.Context(), &pingv1.PingRequest{Text: text})
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
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv, connecthttp.WithCompressMinBytes(8))

	server := memhttptest.NewServer(t, mux)
	client := server.Client()

	getPingResponse := func(t *testing.T, pingText string) *http.Response {
		t.Helper()
		request := &pingv1.PingRequest{Text: pingText}
		requestBytes, err := proto.Marshal(request)
		assert.Nil(t, err)
		req, err := http.NewRequestWithContext(
			t.Context(),
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
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv, connecthttp.WithCompressor(deflateCompressor{}))

	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
		server.URL(),
		connecthttp.WithCompressor(deflateCompressor{}),
		connecthttp.WithSendCompression("deflate"))),
	)
	request := &pingv1.PingRequest{Text: "testing 1..2..3.."}
	response, err := client.Ping(t.Context(), request)
	assert.Nil(t, err)
	assert.Equal(t, response, &pingv1.PingResponse{Text: request.GetText()})
}

func TestClientWithoutGzipSupport(t *testing.T) {
	// See https://github.com/connectrpc/connect-go/pull/349 for why we want to
	// support this. TL;DR is that Microsoft's dapr sidecar can't handle
	// asymmetric compression.
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
		server.URL(),
		connecthttp.WithNoCompression(),
		connecthttp.WithSendGzip())),
	)
	request := &pingv1.PingRequest{Text: "gzip me!"}
	_, err := client.Ping(t.Context(), request)
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
	assert.True(t, strings.Contains(err.Error(), "unknown compression"))
}

func TestInvalidHeaderTimeout(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)
	getPingResponseWithTimeout := func(t *testing.T, timeout string) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(
			t.Context(),
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

func TestHandlerWithReadMaxBytes(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	readMaxBytes := 1024
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	connecthttp.Mount(mux, server, connecthttp.WithReadMaxBytes(readMaxBytes))

	readMaxBytesMatrix := func(t *testing.T, client pingv1connect.PingServiceClient, compressed bool) {
		t.Helper()
		t.Run("equal_read_max", func(t *testing.T) {
			t.Parallel()
			// Serializes to exactly readMaxBytes (1024) - no errors expected
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", 1021)}
			assert.Equal(t, proto.Size(pingRequest), readMaxBytes)
			_, err := client.Ping(t.Context(), pingRequest)
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
			_, err := client.Ping(t.Context(), pingRequest)
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
			_, err := client.Ping(t.Context(), pingRequest)
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
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		readMaxBytesMatrix(t, client, false)
	})
	t.Run("connect_gzip", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithSendCompression("gzip"))))
		readMaxBytesMatrix(t, client, true)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPC())))
		readMaxBytesMatrix(t, client, false)
	})
	t.Run("grpc_gzip", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPC(), connecthttp.WithSendCompression("gzip"))))
		readMaxBytesMatrix(t, client, true)
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPCWeb())))
		readMaxBytesMatrix(t, client, false)
	})
	t.Run("grpcweb_gzip", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPCWeb(), connecthttp.WithSendCompression("gzip"))))
		readMaxBytesMatrix(t, client, true)
	})
}

func TestHandlerWithHTTPMaxBytes(t *testing.T) {
	// This is similar to Connect's own ReadMaxBytes option, but applied to the
	// whole stream using the stdlib's http.MaxBytesHandler.
	t.Parallel()
	const readMaxBytes = 128
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	innerMux := http.NewServeMux()
	connecthttp.Mount(innerMux, srv)
	mux := http.NewServeMux()
	mux.Handle("/", http.MaxBytesHandler(innerMux, readMaxBytes))
	run := func(t *testing.T, client pingv1connect.PingServiceClient, compressed bool) {
		t.Helper()
		t.Run("below_read_max", func(t *testing.T) {
			t.Parallel()
			_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
			assert.Nil(t, err)
		})
		t.Run("just_above_max", func(t *testing.T) {
			t.Parallel()
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", readMaxBytes*10)}
			_, err := client.Ping(t.Context(), pingRequest)
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
			_, err := client.Ping(t.Context(), pingRequest)
			assert.NotNil(t, err, assert.Sprintf("expected non-nil error for large message"))
			assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
		})
	}
	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		run(t, client, false)
	})
	t.Run("connect_gzip", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithSendCompression("gzip"))))
		run(t, client, true)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPC())))
		run(t, client, false)
	})
	t.Run("grpc_gzip", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPC(), connecthttp.WithSendCompression("gzip"))))
		run(t, client, true)
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPCWeb())))
		run(t, client, false)
	})
	t.Run("grpcweb_gzip", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPCWeb(), connecthttp.WithSendCompression("gzip"))))
		run(t, client, true)
	})
}

func TestClientWithReadMaxBytes(t *testing.T) {
	t.Parallel()
	createServer := func(tb testing.TB, enableCompression bool) *memhttp.Server {
		tb.Helper()
		mux := http.NewServeMux()
		var compressionOption connecthttp.Option
		if enableCompression {
			compressionOption = connecthttp.WithCompressMinBytes(1)
		} else {
			compressionOption = connecthttp.WithCompressMinBytes(math.MaxInt)
		}
		srv := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
		connecthttp.Mount(mux, srv, compressionOption)
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
			_, err := client.Ping(t.Context(), pingRequest)
			assert.Nil(t, err)
		})
		t.Run("read_max_plus_one", func(t *testing.T) {
			t.Parallel()
			// Serializes to readMaxBytes+1 (1025) - expect resource exhausted.
			// This will be over the limit after decompression but under with compression.
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", 1022)}
			_, err := client.Ping(t.Context(), pingRequest)
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
			_, err := client.Ping(t.Context(), pingRequest)
			assert.NotNil(t, err, assert.Sprintf("expected non-nil error for large message"))
			assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
			assert.Equal(t, err.Error(), fmt.Sprintf("resource_exhausted: message size %d is larger than configured max %d", expectedSize, readMaxBytes))
		})
	}
	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(serverUncompressed.Client(), serverUncompressed.URL(), connecthttp.WithReadMaxBytes(readMaxBytes))))
		readMaxBytesMatrix(t, client, false)
	})
	t.Run("connect_gzip", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(serverCompressed.Client(), serverCompressed.URL(), connecthttp.WithReadMaxBytes(readMaxBytes))))
		readMaxBytesMatrix(t, client, true)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(serverUncompressed.Client(), serverUncompressed.URL(), connecthttp.WithReadMaxBytes(readMaxBytes), connecthttp.WithGRPC())))
		readMaxBytesMatrix(t, client, false)
	})
	t.Run("grpc_gzip", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(serverCompressed.Client(), serverCompressed.URL(), connecthttp.WithReadMaxBytes(readMaxBytes), connecthttp.WithGRPC())))
		readMaxBytesMatrix(t, client, true)
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(serverUncompressed.Client(), serverUncompressed.URL(), connecthttp.WithReadMaxBytes(readMaxBytes), connecthttp.WithGRPCWeb())))
		readMaxBytesMatrix(t, client, false)
	})
	t.Run("grpcweb_gzip", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(serverCompressed.Client(), serverCompressed.URL(), connecthttp.WithReadMaxBytes(readMaxBytes), connecthttp.WithGRPCWeb())))
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
			_, err := client.Ping(t.Context(), pingRequest)
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
			_, err := client.Ping(t.Context(), pingRequest)
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
			_, err := client.Ping(t.Context(), pingRequest)
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
		options := []connecthttp.Option{connecthttp.WithSendMaxBytes(sendMaxBytes)}
		if compressed {
			options = append(options, connecthttp.WithCompressMinBytes(1))
		} else {
			options = append(options, connecthttp.WithCompressMinBytes(math.MaxInt))
		}
		srv := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
		connecthttp.Mount(mux, srv, options...)

		server := memhttptest.NewServer(t, mux)
		return server
	}
	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t, false, sendMaxBytes)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		sendMaxBytesMatrix(t, client, false)
	})
	t.Run("connect_gzip", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t, true, sendMaxBytes)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		sendMaxBytesMatrix(t, client, true)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t, false, sendMaxBytes)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPC())))
		sendMaxBytesMatrix(t, client, false)
	})
	t.Run("grpc_gzip", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t, true, sendMaxBytes)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPC())))
		sendMaxBytesMatrix(t, client, true)
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t, false, sendMaxBytes)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPCWeb())))
		sendMaxBytesMatrix(t, client, false)
	})
	t.Run("grpcweb_gzip", func(t *testing.T) {
		t.Parallel()
		server := newHTTP2Server(t, true, sendMaxBytes)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPCWeb())))
		sendMaxBytesMatrix(t, client, true)
	})
}

func TestClientWithSendMaxBytes(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)
	sendMaxBytesMatrix := func(t *testing.T, client pingv1connect.PingServiceClient, sendMaxBytes int, compressed bool) {
		t.Helper()
		t.Run("equal_send_max", func(t *testing.T) {
			t.Parallel()
			// Serializes to exactly sendMaxBytes (1024) - no errors expected
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", 1021)}
			assert.Equal(t, proto.Size(pingRequest), sendMaxBytes)
			_, err := client.Ping(t.Context(), pingRequest)
			assert.Nil(t, err)
		})
		t.Run("send_max_plus_one", func(t *testing.T) {
			t.Parallel()
			// Serializes to sendMaxBytes+1 (1025) - expect resource exhausted.
			pingRequest := &pingv1.PingRequest{Text: strings.Repeat("a", 1022)}
			assert.Equal(t, proto.Size(pingRequest), sendMaxBytes+1)
			_, err := client.Ping(t.Context(), pingRequest)
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
			_, err := client.Ping(t.Context(), pingRequest)
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
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithSendMaxBytes(sendMaxBytes))))
		sendMaxBytesMatrix(t, client, sendMaxBytes, false)
	})
	t.Run("connect_gzip", func(t *testing.T) {
		t.Parallel()
		sendMaxBytes := 1024
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithSendMaxBytes(sendMaxBytes), connecthttp.WithSendCompression("gzip"))))
		sendMaxBytesMatrix(t, client, sendMaxBytes, true)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		sendMaxBytes := 1024
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithSendMaxBytes(sendMaxBytes), connecthttp.WithGRPC())))
		sendMaxBytesMatrix(t, client, sendMaxBytes, false)
	})
	t.Run("grpc_gzip", func(t *testing.T) {
		t.Parallel()
		sendMaxBytes := 1024
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithSendMaxBytes(sendMaxBytes), connecthttp.WithGRPC(), connecthttp.WithSendCompression("gzip"))))
		sendMaxBytesMatrix(t, client, sendMaxBytes, true)
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		sendMaxBytes := 1024
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithSendMaxBytes(sendMaxBytes), connecthttp.WithGRPCWeb())))
		sendMaxBytesMatrix(t, client, sendMaxBytes, false)
	})
	t.Run("grpcweb_gzip", func(t *testing.T) {
		t.Parallel()
		sendMaxBytes := 1024
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithSendMaxBytes(sendMaxBytes), connecthttp.WithGRPCWeb(), connecthttp.WithSendCompression("gzip"))))
		sendMaxBytesMatrix(t, client, sendMaxBytes, true)
	})
}
func TestBidiStreamServerSendsFirstMessage(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, opts ...connecthttp.Option) {
		t.Helper()
		headersSent := make(chan struct{})
		pingServer := &pluggablePingServer{
			cumSum: func(_ context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
				close(headersSent)
				return nil
			},
		}
		mux := http.NewServeMux()
		srv := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(srv, pingServer)
		connecthttp.Mount(mux, srv)
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
			server.URL(),
			opts...), assertPeerInterceptor(t)),
		)
		stream, err := client.CumSum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			assert.Nil(t, stream.CloseSend())
			assert.Nil(t, stream.Close())
		})
		assert.Nil(t, stream.SendHeaders())
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
		run(t, connecthttp.WithGRPC())
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		run(t, connecthttp.WithGRPCWeb())
	})
}

func TestStreamForServer(t *testing.T) {
	t.Parallel()
	newPingClient := func(t *testing.T, pingServer pingv1connect.PingServiceHandler) pingv1connect.PingServiceClient {
		t.Helper()
		mux := http.NewServeMux()
		srv := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(srv, pingServer)
		connecthttp.Mount(mux, srv)
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
			server.URL())),
		)
		return client
	}
	t.Run("not-proto-message", func(t *testing.T) {
		t.Parallel()

		mux := http.NewServeMux()
		srv := connect.NewServer()
		srv.Register(connect.Method{
			Spec: connect.Spec{
				Procedure:  pingv1connect.PingServiceCumSumProcedure,
				StreamType: connect.StreamTypeBidi,
			},
			Handler: func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
				return stream.Send("a-string")
			},
		})
		connecthttp.Mount(mux, srv)
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(
			connecthttp.NewTransport(server.Client(), server.URL()),
		))
		stream, err := client.CumSum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		assert.Nil(t, stream.Send(&pingv1.CumSumRequest{}))
		_, err = stream.Receive()
		assert.NotNil(t, err)
		assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
		assert.Nil(t, stream.CloseSend())
	})
	t.Run("get-spec", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			cumSum: func(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
				info, _ := connect.CallInfoForServerContext(ctx)
				assert.Equal(t, info.Spec.StreamType, connect.StreamTypeBidi)
				assert.Equal(t, info.Spec.Procedure, pingv1connect.PingServiceCumSumProcedure)
				return nil
			},
		})
		stream, err := client.CumSum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		assert.Nil(t, stream.SendHeaders())
		assert.Nil(t, stream.CloseSend())
	})
	t.Run("server-stream", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			countUp: func(ctx context.Context, req *pingv1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
				info, _ := connect.CallInfoForServerContext(ctx)
				assert.Equal(t, info.Spec.StreamType, connect.StreamTypeServer)
				assert.Equal(t, info.Spec.Procedure, pingv1connect.PingServiceCountUpProcedure)
				assert.Nil(t, stream.Send(&pingv1.CountUpResponse{Number: 1}))
				return nil
			},
		})
		stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{})
		assert.Nil(t, err)
		assert.NotNil(t, stream)
		assert.Nil(t, stream.Close())
	})
	t.Run("server-stream-send", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			countUp: func(ctx context.Context, req *pingv1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
				assert.Nil(t, stream.Send(&pingv1.CountUpResponse{Number: 1}))
				return nil
			},
		})
		stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{})
		assert.Nil(t, err)
		msg, err := stream.Receive()
		assert.Nil(t, err)
		assert.NotNil(t, msg)
		assert.Equal(t, msg.GetNumber(), int64(1))
		assert.Nil(t, stream.Close())
	})
	t.Run("client-stream", func(t *testing.T) {
		t.Parallel()
		client := newPingClient(t, &pluggablePingServer{
			sum: func(ctx context.Context, stream pingv1connect.PingServiceSumServerStream) (*pingv1.SumResponse, error) {
				info, _ := connect.CallInfoForServerContext(ctx)
				assert.Equal(t, info.Spec.StreamType, connect.StreamTypeClient)
				assert.Equal(t, info.Spec.Procedure, pingv1connect.PingServiceSumProcedure)
				msg, err := stream.Receive()
				assert.Nil(t, err)
				assert.NotNil(t, msg)
				assert.Equal(t, msg.GetNumber(), int64(1))
				return &pingv1.SumResponse{Sum: 1}, nil
			},
		})
		cstream, err := client.Sum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		assert.Nil(t, cstream.Send(&pingv1.SumRequest{Number: 1}))
		res, err := cstream.CloseAndReceive()
		assert.Nil(t, err)
		assert.NotNil(t, res)
		assert.Equal(t, res.GetSum(), int64(1))
	})
}

func TestConnectHTTPErrorCodes(t *testing.T) {
	t.Parallel()
	checkHTTPStatus := func(t *testing.T, connectCode connect.Code, wantHttpStatus int) {
		t.Helper()
		mux := http.NewServeMux()
		pluggableServer := &pluggablePingServer{
			ping: func(_ context.Context, _ *pingv1.PingRequest) (*pingv1.PingResponse, error) {
				return nil, connect.NewError(connectCode, "error")
			},
		}
		srv := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(srv, pluggableServer)
		connecthttp.Mount(mux, srv)
		server := memhttptest.NewServer(t, mux)
		req, err := http.NewRequestWithContext(
			t.Context(),
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
		connectClient := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		connectResp, err := connectClient.Ping(t.Context(), &pingv1.PingRequest{})
		assert.NotNil(t, err)
		assert.Nil(t, connectResp)
	}
	t.Run("connect.CodeCanceled-499", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeCanceled, 499)
	})
	t.Run("connect.CodeUnknown-500", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeUnknown, 500)
	})
	t.Run("connect.CodeInvalidArgument-400", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeInvalidArgument, 400)
	})
	t.Run("connect.CodeDeadlineExceeded-504", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeDeadlineExceeded, 504)
	})
	t.Run("connect.CodeNotFound-404", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeNotFound, 404)
	})
	t.Run("connect.CodeAlreadyExists-409", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeAlreadyExists, 409)
	})
	t.Run("connect.CodePermissionDenied-403", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodePermissionDenied, 403)
	})
	t.Run("connect.CodeResourceExhausted-429", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeResourceExhausted, 429)
	})
	t.Run("connect.CodeFailedPrecondition-400", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeFailedPrecondition, 400)
	})
	t.Run("connect.CodeAborted-409", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeAborted, 409)
	})
	t.Run("connect.CodeOutOfRange-400", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeOutOfRange, 400)
	})
	t.Run("connect.CodeUnimplemented-501", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeUnimplemented, 501)
	})
	t.Run("connect.CodeInternal-500", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeInternal, 500)
	})
	t.Run("connect.CodeUnavailable-503", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeUnavailable, 503)
	})
	t.Run("connect.CodeDataLoss-500", func(t *testing.T) {
		t.Parallel()
		checkHTTPStatus(t, connect.CodeDataLoss, 500)
	})
	t.Run("connect.CodeUnauthenticated-401", func(t *testing.T) {
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
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv, connecthttp.WithCompressor(failCompressor{}))
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(
		connecthttp.NewTransport(server.Client(), server.URL(),
			connecthttp.WithCompressor(failCompressor{}),
			connecthttp.WithSendCompression(failCompressor{}.Name()),
		),
	))
	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Text: "ping"})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
}

func TestUnflushableResponseWriter(t *testing.T) {
	t.Parallel()
	assertIsFlusherErr := func(t *testing.T, err error) {
		t.Helper()
		if !assert.NotNil(t, err) {
			return
		}
		assert.Equal(t, connect.CodeOf(err), connect.CodeInternal, assert.Sprintf("got %v", err))
		assert.True(
			t,
			strings.HasSuffix(err.Error(), "unflushableWriter does not implement http.Flusher"),
			assert.Sprintf("error doesn't reference http.Flusher: %s", err.Error()),
		)
	}
	innerMux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(innerMux, srv)
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerMux.ServeHTTP(&unflushableWriter{w}, r)
	}))
	server := memhttptest.NewServer(t, mux)

	tests := []struct {
		name    string
		options []connecthttp.Option
	}{
		{"connect", nil},
		{"grpc", []connecthttp.Option{connecthttp.WithGRPC()}},
		{"grpcweb", []connecthttp.Option{connecthttp.WithGRPCWeb()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pingclient := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), tt.options...)))
			stream, err := pingclient.CountUp(
				t.Context(),
				&pingv1.CountUpRequest{Number: 5},
			)
			if err != nil {
				assertIsFlusherErr(t, err)
				return
			}
			_, err = stream.Receive()
			if err != nil {
				assertIsFlusherErr(t, err)
			}
		})
	}
}

func TestGRPCErrorMetadataIsTrailersOnly(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
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
		t.Context(),
		http.MethodPost,
		server.URL()+pingv1connect.PingServiceFailProcedure,
		bytes.NewReader(body),
	)
	assert.Nil(t, err)
	req.Header.Set("Content-Type", "application/grpc")
	for _, el := range expectedHeaderValues {
		req.Header.Add(clientHeader, el)
	}
	res, err := server.Client().Do(req)
	assert.Nil(t, err)
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, res.Header.Get("Content-Type"), "application/grpc")
	// pingServer.Fail sets handlerHeader on the response header and
	// handlerTrailer on the response trailer. gRPC sends leading metadata as
	// HTTP headers and trailing metadata as HTTP trailers.
	assert.NotZero(t, res.Header.Get(handlerHeader))
	assert.Zero(t, res.Header.Get(handlerTrailer))
	_, err = io.Copy(io.Discard, res.Body)
	assert.Nil(t, err)
	assert.Nil(t, res.Body.Close())
	assert.Zero(t, res.Trailer.Get(handlerHeader))
	assert.NotZero(t, res.Trailer.Get(handlerTrailer))
}

func TestConnectProtocolHeaderSentByDefault(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv, connecthttp.WithRequireConnectProtocolHeader())
	server := memhttptest.NewServer(t, mux)

	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
	_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
	assert.Nil(t, err)

	stream, err := client.CumSum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{}))
	_, err = stream.Receive()
	assert.Nil(t, err)
	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())
}

func TestConnectProtocolHeaderRequired(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv, connecthttp.WithRequireConnectProtocolHeader())

	server := memhttptest.NewServer(t, mux)

	tests := []struct {
		headers http.Header
	}{
		{http.Header{}},
		{http.Header{"Connect-Protocol-Version": []string{"0"}}},
	}
	for _, tcase := range tests {
		req, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			server.URL()+pingv1connect.PingServicePingProcedure,
			strings.NewReader("{}"),
		)
		assert.Nil(t, err)
		req.Header.Set("Content-Type", "application/json")
		maps.Copy(req.Header, tcase.headers)
		response, err := server.Client().Do(req)
		assert.Nil(t, err)
		assert.Nil(t, response.Body.Close())
		assert.Equal(t, response.StatusCode, http.StatusBadRequest)
	}
}

func TestUserAgent(t *testing.T) {
	t.Parallel()

	const customAgent = "custom"
	protocols := []struct {
		name string
		opts []connecthttp.Option
	}{
		{"connect", nil},
		{"grpc", []connecthttp.Option{connecthttp.WithGRPC()}},
		{"grpcweb", []connecthttp.Option{connecthttp.WithGRPCWeb()}},
	}
	headers := []struct {
		name string
		// set mutates the outgoing request header. A nil func leaves the
		// User-Agent unset, so the framework should supply its own default.
		set func(*connect.Header)
		// check verifies what the server received for the User-Agent header.
		check func(t *testing.T, values []string, ok bool)
	}{
		{
			// With no User-Agent provided, the framework injects its default.
			name: "unset",
			set:  nil,
			check: func(t *testing.T, values []string, ok bool) {
				t.Helper()
				assert.True(t, ok)
				assert.Equal(t, len(values), 1)
				assert.NotZero(t, values[0])
			},
		},
		{
			// A user-provided User-Agent must not be clobbered.
			name: "custom",
			set:  func(h *connect.Header) { h.Set("User-Agent", customAgent) },
			check: func(t *testing.T, values []string, _ bool) {
				t.Helper()
				assert.Equal(t, values, []string{customAgent})
			},
		},
		{
			// An explicit empty value suppresses the default; the server
			// should see no User-Agent header at all.
			name: "empty-string",
			set:  func(h *connect.Header) { h.Set("User-Agent", "") },
			check: func(t *testing.T, _ []string, ok bool) {
				t.Helper()
				assert.False(t, ok)
			},
		},
		{
			// An explicit nil slice also suppresses the default.
			name: "nil-slice",
			set:  func(h *connect.Header) { h.SetValues("User-Agent", nil) },
			check: func(t *testing.T, _ []string, ok bool) {
				t.Helper()
				assert.False(t, ok)
			},
		},
	}
	for _, protocol := range protocols {
		for _, header := range headers {
			t.Run(protocol.name+"/"+header.name, func(t *testing.T) {
				t.Parallel()
				mux := http.NewServeMux()
				srv := connect.NewServer()
				pingv1connect.RegisterPingServiceHandler(srv, &pluggablePingServer{
					ping: func(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
						info, _ := connect.CallInfoForServerContext(ctx)
						values := info.RequestHeader().Values("User-Agent")
						ok := info.RequestHeader().Has("User-Agent")
						t.Log(values, ok)
						header.check(t, values, ok)
						return &pingv1.PingResponse{Number: req.GetNumber()}, nil
					},
				})
				connecthttp.Mount(mux, srv)

				server := memhttptest.NewServer(t, mux)
				client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), protocol.opts...)))
				ctx := t.Context()
				if header.set != nil {
					var callInfo *connect.CallInfo
					ctx, callInfo = connect.NewClientContext(ctx)
					header.set(callInfo.RequestHeader())
				}
				_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 42})
				assert.Nil(t, err)
			})
		}
	}
}

func TestWebXUserAgent(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, &pluggablePingServer{
		ping: func(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
			info, _ := connect.CallInfoForServerContext(ctx)
			agent := info.RequestHeader().Get("User-Agent")
			assert.NotZero(t, agent)
			xAgent := info.RequestHeader().Get("X-User-Agent")
			assert.Equal(
				t,
				xAgent,
				agent,
			)
			return &pingv1.PingResponse{Number: req.GetNumber()}, nil
		},
	})
	connecthttp.Mount(mux, srv)

	server := memhttptest.NewServer(t, mux)

	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPCWeb())))
	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 42})
	assert.Nil(t, err)
}

func TestBidiOverHTTP1(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)

	// Clients expecting a full-duplex connection that end up with a simplex
	// HTTP/1.1 connection shouldn't hang. Instead, the server should close the
	// TCP connection.
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: server.TransportHTTP1()},
		server.URL())),
	)
	stream, err := client.CumSum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Stream creates an async request, can error on Send or Receive.
	if err := stream.Send(&pingv1.CumSumRequest{Number: 2}); err != nil {
		assert.ErrorIs(t, err, io.EOF)
	}
	_, err = stream.Receive()
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
	assert.Equal(t, err.Error(), "unknown: HTTP status 505 HTTP Version Not Supported")
	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())
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
		options    []connecthttp.Option
		expectCode connect.Code
		expectMsg  string
	}{{
		name:    "connect_missing_end",
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON)},
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
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON), connecthttp.WithGRPC()},
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
		name:    "grpc_missing_status",
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON), connecthttp.WithGRPC()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			header := responseWriter.Header()
			header.Set("Content-Type", "application/grpc+json")
			_, err := responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write(payload)
			assert.Nil(t, err)
			// Trailers exist, just no status. So error will be unknown instead of internal.
			responseWriter.Header().Set(http.TrailerPrefix+"grpc-message", "foo")
		},
		expectCode: connect.CodeUnknown,
		expectMsg:  "unknown: protocol error: no Grpc-Status trailer: unexpected EOF",
	}, {
		name:    "grpc-web_missing_end",
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON), connecthttp.WithGRPCWeb()},
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
		name:    "grpc-web_missing_status",
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON), connecthttp.WithGRPCWeb()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			header := responseWriter.Header()
			header.Set("Content-Type", "application/grpc-web+json")
			_, err := responseWriter.Write(head[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write(payload)
			assert.Nil(t, err)
			// Trailers exist, just no status. So error will be unknown instead of internal.
			_, err = responseWriter.Write([]byte{128}) // end-stream flag
			assert.Nil(t, err)
			endStream := "grpc-message: foo\r\n"
			var length [4]byte
			binary.BigEndian.PutUint32(length[:], uint32(len(endStream)))
			_, err = responseWriter.Write(length[:])
			assert.Nil(t, err)
			_, err = responseWriter.Write([]byte(endStream))
			assert.Nil(t, err)
		},
		expectCode: connect.CodeUnknown,
		expectMsg:  "unknown: protocol error: no Grpc-Status trailer: unexpected EOF",
	}, {
		name:    "connect_partial_payload",
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON)},
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
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON), connecthttp.WithGRPC()},
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
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON), connecthttp.WithGRPCWeb()},
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
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON)},
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
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON), connecthttp.WithGRPC()},
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
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON), connecthttp.WithGRPCWeb()},
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
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON)},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			responseWriter.Header().Set("Content-Type", "application/connect+json")
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
		options: []connecthttp.Option{connecthttp.WithSendCodec(connect.CodecNameJSON), connecthttp.WithGRPCWeb()},
		handler: func(responseWriter http.ResponseWriter, _ *http.Request) {
			responseWriter.Header().Set("Content-Type", "application/grpc-web+json")
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
		t.Run(testcase.name, func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
				server.URL(),
				testcase.options...)))
			const upTo = 2
			callInfo.RequestHeader().Set("Test-Case", t.Name())
			stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: upTo})
			assert.Nil(t, err)
			var streamErr error
			for range upTo {
				_, err := stream.Receive()
				if err != nil {
					streamErr = err
					break
				}
			}
			if streamErr == nil {
				_, streamErr = stream.Receive()
			}
			assert.NotNil(t, streamErr)
			assert.Equal(t, connect.CodeOf(streamErr), testcase.expectCode)
			assert.Equal(t, streamErr.Error(), testcase.expectMsg)
		})
	}
}

// TestClientDisconnect tests that the handler receives a connect.CodeCanceled error when
// the client abruptly disconnects.
func TestClientDisconnect(t *testing.T) {
	t.Parallel()
	type httpRoundTripFunc func(server *memhttp.Server, clientConn *net.Conn, onError chan struct{}) http.RoundTripper
	http1RoundTripper := func(server *memhttp.Server, clientConn *net.Conn, onError chan struct{}) http.RoundTripper {
		transport := server.TransportHTTP1()
		dialContext := transport.DialContext
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialContext(ctx, network, addr)
			if err != nil {
				close(onError)
				return nil, err
			}
			*clientConn = conn // Capture the client connection.
			return conn, nil
		}
		return transport
	}
	http2RoundTripper := func(server *memhttp.Server, clientConn *net.Conn, onError chan struct{}) http.RoundTripper {
		transport := server.Transport()
		dialContext := transport.DialContext
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialContext(ctx, network, addr)
			if err != nil {
				close(onError)
				return nil, err
			}
			*clientConn = conn // Capture the client connection.
			return conn, nil
		}
		return transport
	}
	testTransportClosure := func(t *testing.T, captureTransport httpRoundTripFunc) { //nolint:thelper
		t.Run("handler_reads", func(t *testing.T) {
			t.Parallel()
			var (
				handlerReceiveErr error
				handlerContextErr error
				gotRequest        = make(chan struct{})
				gotResponse       = make(chan struct{})
			)
			pingServer := &pluggablePingServer{
				sum: func(ctx context.Context, stream pingv1connect.PingServiceSumServerStream) (*pingv1.SumResponse, error) {
					close(gotRequest)
					for {
						_, err := stream.Receive()
						if err != nil {
							if !errors.Is(err, io.EOF) {
								handlerReceiveErr = err
							}
							break
						}
						// Do nothing
					}
					<-ctx.Done() // Context cancel is asynchronous, wait for cancel
					handlerContextErr = ctx.Err()
					close(gotResponse)
					return &pingv1.SumResponse{}, nil
				},
			}
			mux := http.NewServeMux()
			srv := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(srv, pingServer)
			connecthttp.Mount(mux, srv)
			server := memhttptest.NewServer(t, mux)
			var clientConn net.Conn
			transport := captureTransport(server, &clientConn, gotRequest)
			serverClient := &http.Client{Transport: transport}
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(serverClient, server.URL())))
			stream, err := client.Sum(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			// Send header.
			assert.Nil(t, stream.Send(nil))
			<-gotRequest
			// Client abruptly disconnects.
			if !assert.NotNil(t, clientConn) {
				return
			}
			assert.Nil(t, clientConn.Close())
			_, err = stream.CloseAndReceive()
			assert.NotNil(t, err)
			<-gotResponse
			assert.NotNil(t, handlerReceiveErr)
			gotCode := connect.CodeOf(handlerReceiveErr)
			assert.True(
				t,
				gotCode == connect.CodeCanceled || gotCode == connect.CodeInvalidArgument,
				assert.Sprintf("got %v", handlerReceiveErr),
			)
			assert.ErrorIs(t, handlerContextErr, context.Canceled)
		})
		t.Run("handler_writes", func(t *testing.T) {
			t.Parallel()
			var (
				handlerReceiveErr error
				handlerContextErr error
				gotRequest        = make(chan struct{})
				gotResponse       = make(chan struct{})
			)
			pingServer := &pluggablePingServer{
				countUp: func(ctx context.Context, _ *pingv1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
					close(gotRequest)
					var err error
					for err == nil {
						err = stream.Send(&pingv1.CountUpResponse{})
					}
					handlerReceiveErr = err
					<-ctx.Done() // Context cancel is asynchronous, wait for cancel
					handlerContextErr = ctx.Err()
					close(gotResponse)
					return nil
				},
			}
			mux := http.NewServeMux()
			srv := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(srv, pingServer)
			connecthttp.Mount(mux, srv)
			server := memhttptest.NewServer(t, mux)
			var clientConn net.Conn
			transport := captureTransport(server, &clientConn, gotRequest)
			serverClient := &http.Client{Transport: transport}
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(serverClient, server.URL())))
			stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{})
			if !assert.Nil(t, err) {
				return
			}
			<-gotRequest
			// Client abruptly disconnects.
			if !assert.NotNil(t, clientConn) {
				return
			}
			assert.Nil(t, clientConn.Close())
			var streamErr error
			for {
				_, err := stream.Receive()
				if err != nil {
					if !errors.Is(err, io.EOF) {
						streamErr = err
					}
					break
				}
				// Do nothing
			}
			assert.NotNil(t, streamErr)
			<-gotResponse
			assert.NotNil(t, handlerReceiveErr)
			assert.Equal(t, connect.CodeOf(handlerReceiveErr), connect.CodeCanceled)
			assert.ErrorIs(t, handlerContextErr, context.Canceled)
		})
	}
	t.Run("http1", func(t *testing.T) {
		t.Parallel()
		testTransportClosure(t, http1RoundTripper)
	})
	t.Run("http2", func(t *testing.T) {
		t.Parallel()
		testTransportClosure(t, http2RoundTripper)
	})
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

// TestSetProtocolHeaders tests that headers required by the protocols are set
// overriding user provided headers.
func TestSetProtocolHeaders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		clientOption      connecthttp.Option
		expectContentType string
	}{{
		name:              "connect",
		expectContentType: "application/proto",
	}, {
		name:              "grpc",
		clientOption:      connecthttp.WithGRPC(),
		expectContentType: "application/grpc",
	}, {
		name:              "grpcweb",
		clientOption:      connecthttp.WithGRPCWeb(),
		expectContentType: "application/grpc-web+proto",
	}}
	for _, tt := range tests {
		testcase := tt
		t.Run(testcase.name, func(t *testing.T) {
			t.Parallel()
			pingServer := &pingServer{}
			mux := http.NewServeMux()
			srv := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(srv, pingServer)
			connecthttp.Mount(mux, srv)
			server := memhttptest.NewServer(t, mux)

			clientOpts := []connecthttp.Option{}
			if testcase.clientOption == nil {
				// Use a different protocol to test the override.
				clientOpts = append(clientOpts, connecthttp.WithGRPC())
			}
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), clientOpts...)))

			pingProxyServer := &pluggablePingServer{
				ping: func(ctx context.Context, request *pingv1.PingRequest) (*pingv1.PingResponse, error) {
					return client.Ping(ctx, request)
				},
			}
			proxyMux := http.NewServeMux()
			srv2 := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(srv2, pingProxyServer)
			connecthttp.Mount(proxyMux, srv2)
			proxyServer := memhttptest.NewServer(t, proxyMux)

			proxyClientOpts := []connecthttp.Option{}
			if testcase.clientOption != nil {
				proxyClientOpts = append(proxyClientOpts, testcase.clientOption)
			}
			proxyClient := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(proxyServer.Client(), proxyServer.URL(), proxyClientOpts...)))

			ctx, callInfo := connect.NewClientContext(t.Context())
			callInfo.RequestHeader().Set("X-Test", t.Name())
			_, err := proxyClient.Ping(ctx, &pingv1.PingRequest{Number: 42})
			if !assert.Nil(t, err) {
				return
			}
			// Assert the Content-Type is set for the proxy clients protocol and not the client's.
			contentType := callInfo.ResponseHeader().Get("Content-Type")
			assert.Equal(t, contentType, testcase.expectContentType)
			assert.Equal(t, len(callInfo.ResponseHeader().Values("Content-Type")), 1)
		})
	}
}

func TestCallInfoHeadersOnError(t *testing.T) {
	t.Parallel()

	handler := &pluggablePingServer{
		ping: func(ctx context.Context, request *pingv1.PingRequest) (*pingv1.PingResponse, error) {
			callInfo, _ := connect.CallInfoForServerContext(ctx)
			if request.GetNumber() < 0 {
				callInfo.ResponseHeader().Set("x-custom-key", "ping-error")
				callInfo.ResponseHeader().Set("x-header-only", "should-not-be-in-trailers")
				callInfo.ResponseTrailer().Set("x-trailer-only", "should-not-be-in-headers")
				return nil, connect.NewError(connect.CodeInvalidArgument, "")
			}
			callInfo.ResponseHeader().Set("x-custom-key", "ping-success")
			callInfo.ResponseHeader().Set("x-success-header", "in-headers")
			callInfo.ResponseTrailer().Set("x-success-trailer", "in-trailers")
			return &pingv1.PingResponse{Number: request.GetNumber()}, nil
		},
		sum: func(ctx context.Context, stream pingv1connect.PingServiceSumServerStream) (*pingv1.SumResponse, error) {
			callInfo, _ := connect.CallInfoForServerContext(ctx)
			var sum int64
			for {
				msg, err := stream.Receive()
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					return nil, err
				}
				if msg.GetNumber() < 0 {
					callInfo.ResponseHeader().Set("x-custom-key", "sum-error")
					callInfo.ResponseHeader().Set("x-header-only", "should-not-be-in-trailers")
					callInfo.ResponseTrailer().Set("x-trailer-only", "should-not-be-in-headers")
					return nil, connect.NewError(connect.CodeInvalidArgument, "")
				}
				sum += msg.GetNumber()
			}
			callInfo.ResponseHeader().Set("x-custom-key", "sum-success")
			callInfo.ResponseHeader().Set("x-success-header", "in-headers")
			callInfo.ResponseTrailer().Set("x-success-trailer", "in-trailers")
			return &pingv1.SumResponse{Sum: sum}, nil
		},
		countUp: func(ctx context.Context, request *pingv1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
			callInfo, _ := connect.CallInfoForServerContext(ctx)
			if request.GetNumber() < 0 {
				callInfo.ResponseHeader().Set("x-custom-key", "countup-error")
				callInfo.ResponseHeader().Set("x-header-only", "should-not-be-in-trailers")
				callInfo.ResponseTrailer().Set("x-trailer-only", "should-not-be-in-headers")
				return connect.NewError(connect.CodeInvalidArgument, "")
			}
			callInfo.ResponseHeader().Set("x-custom-key", "countup-success")
			callInfo.ResponseHeader().Set("x-success-header", "in-headers")
			callInfo.ResponseTrailer().Set("x-success-trailer", "in-trailers")
			for number := int64(1); number <= request.GetNumber(); number++ {
				// Simulate an error after sending some responses (for testing trailers).
				if number == 3 && request.GetNumber() == 5 {
					callInfo.ResponseTrailer().Set("x-error-trailer", "error-after-streaming")
					return connect.NewError(connect.CodeInternal, "")
				}
				if err := stream.Send(&pingv1.CountUpResponse{Number: number}); err != nil {
					return err
				}
			}
			return nil
		},
		cumSum: func(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
			callInfo, _ := connect.CallInfoForServerContext(ctx)
			callInfo.ResponseHeader().Set("x-custom-key", "cumsum-success")
			callInfo.ResponseHeader().Set("x-success-header", "in-headers")
			callInfo.ResponseTrailer().Set("x-success-trailer", "in-trailers")
			var sum int64
			for {
				req, err := stream.Receive()
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					return err
				}
				if req.GetNumber() == -99 {
					// Error after successful exchanges (for testing trailers).
					callInfo.ResponseTrailer().Set("x-error-trailer", "error-after-streaming")
					return connect.NewError(connect.CodeInternal, "")
				}
				if req.GetNumber() < 0 {
					callInfo.ResponseHeader().Set("x-custom-key", "cumsum-error")
					callInfo.ResponseHeader().Set("x-header-only", "should-not-be-in-trailers")
					callInfo.ResponseTrailer().Set("x-trailer-only", "should-not-be-in-headers")
					return connect.NewError(connect.CodeInvalidArgument, "")
				}
				sum += req.GetNumber()
				if err := stream.Send(&pingv1.CumSumResponse{Sum: sum}); err != nil {
					return err
				}
			}
			return nil
		},
	}

	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, handler)
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)

	testCallInfoHeaders := func(t *testing.T, client pingv1connect.PingServiceClient) { //nolint:thelper
		t.Run("unary_ping_success", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			response, err := client.Ping(ctx, &pingv1.PingRequest{Number: 1})
			assert.Nil(t, err)
			assert.NotNil(t, response)

			assert.True(t, compareValues(callInfo.ResponseHeader().Values("x-custom-key"), []string{"ping-success"}))
			assert.True(t, compareValues(callInfo.ResponseHeader().Values("x-success-header"), []string{"in-headers"}))
			assert.True(t, compareValues(callInfo.ResponseTrailer().Values("x-success-trailer"), []string{"in-trailers"}))
			assert.Equal(t, len(callInfo.ResponseTrailer().Values("x-success-header")), 0)
			assert.Equal(t, len(callInfo.ResponseHeader().Values("x-success-trailer")), 0)
		})
		t.Run("unary_ping_error", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			response, err := client.Ping(ctx, &pingv1.PingRequest{Number: -1})
			assert.NotNil(t, err)
			assert.Nil(t, response)

			assert.True(t, compareValues(metaValues(callInfo, "x-custom-key"), []string{"ping-error"}))
			assert.True(t, compareValues(metaValues(callInfo, "x-header-only"), []string{"should-not-be-in-trailers"}))
			assert.True(t, compareValues(callInfo.ResponseTrailer().Values("x-trailer-only"), []string{"should-not-be-in-headers"}))
			assert.Equal(t, len(callInfo.ResponseHeader().Values("x-trailer-only")), 0)
		})
		t.Run("client_stream_sum_success", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			stream, err := client.Sum(ctx)
			assert.Nil(t, err)
			assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 1}))
			assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 2}))
			assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 3}))
			response, err := stream.CloseAndReceive()
			assert.Nil(t, err)
			assert.NotNil(t, response)

			assert.True(t, compareValues(callInfo.ResponseHeader().Values("x-custom-key"), []string{"sum-success"}))
			assert.True(t, compareValues(callInfo.ResponseHeader().Values("x-success-header"), []string{"in-headers"}))
			assert.True(t, compareValues(callInfo.ResponseTrailer().Values("x-success-trailer"), []string{"in-trailers"}))
			assert.Equal(t, len(callInfo.ResponseTrailer().Values("x-success-header")), 0)
			assert.Equal(t, len(callInfo.ResponseHeader().Values("x-success-trailer")), 0)
		})
		t.Run("client_stream_sum_error", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			stream, err := client.Sum(ctx)
			assert.Nil(t, err)
			assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: -1}))
			response, err := stream.CloseAndReceive()
			assert.NotNil(t, err)
			assert.Nil(t, response)

			assert.True(t, compareValues(metaValues(callInfo, "x-custom-key"), []string{"sum-error"}))
			assert.True(t, compareValues(metaValues(callInfo, "x-header-only"), []string{"should-not-be-in-trailers"}))
			assert.True(t, compareValues(callInfo.ResponseTrailer().Values("x-trailer-only"), []string{"should-not-be-in-headers"}))
			assert.Equal(t, len(callInfo.ResponseHeader().Values("x-trailer-only")), 0)
		})
		t.Run("server_stream_countup_success", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 3})
			assert.Nil(t, err)
			count := 0
			for {
				if _, err := stream.Receive(); err != nil {
					assert.True(t, errors.Is(err, io.EOF))
					break
				}
				count++
			}
			assert.Nil(t, stream.Close())
			assert.Equal(t, count, 3)

			assert.True(t, compareValues(callInfo.ResponseHeader().Values("x-custom-key"), []string{"countup-success"}))
			assert.True(t, compareValues(callInfo.ResponseHeader().Values("x-success-header"), []string{"in-headers"}))
			assert.True(t, compareValues(callInfo.ResponseTrailer().Values("x-success-trailer"), []string{"in-trailers"}))
			assert.Equal(t, len(callInfo.ResponseTrailer().Values("x-success-header")), 0)
			assert.Equal(t, len(callInfo.ResponseHeader().Values("x-success-trailer")), 0)
		})
		t.Run("server_stream_countup_error", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: -1})
			assert.Nil(t, err)
			_, err = stream.Receive()
			assert.NotNil(t, err)
			assert.Nil(t, stream.Close())

			assert.True(t, compareValues(metaValues(callInfo, "x-custom-key"), []string{"countup-error"}))
			assert.True(t, compareValues(metaValues(callInfo, "x-header-only"), []string{"should-not-be-in-trailers"}))
			assert.True(t, compareValues(callInfo.ResponseTrailer().Values("x-trailer-only"), []string{"should-not-be-in-headers"}))
			assert.Equal(t, len(callInfo.ResponseHeader().Values("x-trailer-only")), 0)
		})
		t.Run("bidi_stream_cumsum_success", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			stream, err := client.CumSum(ctx)
			assert.Nil(t, err)
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
			msg1, err := stream.Receive()
			assert.Nil(t, err)
			assert.Equal(t, msg1.GetSum(), int64(1))
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 2}))
			msg2, err := stream.Receive()
			assert.Nil(t, err)
			assert.Equal(t, msg2.GetSum(), int64(3))
			assert.Nil(t, stream.CloseSend())
			_, err = stream.Receive()
			assert.True(t, errors.Is(err, io.EOF))
			assert.Nil(t, stream.Close())

			assert.True(t, compareValues(callInfo.ResponseHeader().Values("x-custom-key"), []string{"cumsum-success"}))
			assert.True(t, compareValues(callInfo.ResponseHeader().Values("x-success-header"), []string{"in-headers"}))
			assert.True(t, compareValues(callInfo.ResponseTrailer().Values("x-success-trailer"), []string{"in-trailers"}))
			assert.Equal(t, len(callInfo.ResponseTrailer().Values("x-success-header")), 0)
			assert.Equal(t, len(callInfo.ResponseHeader().Values("x-success-trailer")), 0)
		})
		t.Run("bidi_stream_cumsum_error", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			stream, err := client.CumSum(ctx)
			assert.Nil(t, err)
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: -1}))
			_, err = stream.Receive()
			assert.NotNil(t, err)
			assert.Nil(t, stream.Close())

			assert.True(t, compareValues(metaValues(callInfo, "x-custom-key"), []string{"cumsum-error"}))
			assert.True(t, compareValues(metaValues(callInfo, "x-header-only"), []string{"should-not-be-in-trailers"}))
			assert.True(t, compareValues(callInfo.ResponseTrailer().Values("x-trailer-only"), []string{"should-not-be-in-headers"}))
			assert.Equal(t, len(callInfo.ResponseHeader().Values("x-trailer-only")), 0)
		})
		t.Run("server_stream_countup_error_after_first_response", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 5})
			assert.Nil(t, err)
			msg1, err := stream.Receive()
			assert.Nil(t, err)
			assert.Equal(t, msg1.GetNumber(), int64(1))
			msg2, err := stream.Receive()
			assert.Nil(t, err)
			assert.Equal(t, msg2.GetNumber(), int64(2))
			_, err = stream.Receive()
			assert.NotNil(t, err)
			assert.Nil(t, stream.Close())

			assert.True(t, compareValues(callInfo.ResponseTrailer().Values("x-error-trailer"), []string{"error-after-streaming"}))
		})
		t.Run("bidi_stream_cumsum_error_after_first_response", func(t *testing.T) {
			t.Parallel()
			ctx, callInfo := connect.NewClientContext(t.Context())
			stream, err := client.CumSum(ctx)
			assert.Nil(t, err)
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
			msg1, err := stream.Receive()
			assert.Nil(t, err)
			assert.Equal(t, msg1.GetSum(), int64(1))
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 2}))
			msg2, err := stream.Receive()
			assert.Nil(t, err)
			assert.Equal(t, msg2.GetSum(), int64(3))
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: -99}))
			_, err = stream.Receive()
			assert.NotNil(t, err)
			assert.Nil(t, stream.Close())

			assert.True(t, compareValues(callInfo.ResponseTrailer().Values("x-error-trailer"), []string{"error-after-streaming"}))
		})
	}

	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		testCallInfoHeaders(t, client)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPC())))
		testCallInfoHeaders(t, client)
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL(), connecthttp.WithGRPCWeb())))
		testCallInfoHeaders(t, client)
	})
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

func (c failCodec) MarshalWrite(_ context.Context, _ io.Writer, _ any) error {
	return errors.New("boom")
}

func (c failCodec) UnmarshalRead(_ context.Context, src io.Reader, message any) error {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return fmt.Errorf("not protobuf: %T", message)
	}
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	return proto.Unmarshal(data, protoMessage)
}

type pluggablePingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	ping    func(context.Context, *pingv1.PingRequest) (*pingv1.PingResponse, error)
	sum     func(context.Context, pingv1connect.PingServiceSumServerStream) (*pingv1.SumResponse, error)
	countUp func(context.Context, *pingv1.CountUpRequest, pingv1connect.PingServiceCountUpServerStream) error
	cumSum  func(context.Context, pingv1connect.PingServiceCumSumServerStream) error
}

func (p *pluggablePingServer) Ping(ctx context.Context, request *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	return p.ping(ctx, request)
}

func (p *pluggablePingServer) Sum(ctx context.Context, stream pingv1connect.PingServiceSumServerStream) (*pingv1.SumResponse, error) {
	return p.sum(ctx, stream)
}

func (p *pluggablePingServer) CountUp(ctx context.Context, req *pingv1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
	return p.countUp(ctx, req, stream)
}

func (p *pluggablePingServer) CumSum(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	return p.cumSum(ctx, stream)
}

type pingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	// Whether to verify metadata sent to the server. Can be used to force an
	// error returned from the server by intentionally sending no metadata.
	checkMetadata       bool
	includeErrorDetails bool
}

func (p pingServer) Ping(ctx context.Context, request *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	if err := validateRequestInfo(info); err != nil {
		return nil, err
	}
	if p.checkMetadata {
		if err := expectMetadata(info.RequestHeader()); err != nil {
			return nil, err
		}
	}
	response := &pingv1.PingResponse{
		Number: request.GetNumber(),
		Text:   request.GetText(),
	}
	copyClientHeaderToResponse(info)
	return response, nil
}

func (p pingServer) Fail(ctx context.Context, request *pingv1.FailRequest) (*pingv1.FailResponse, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	if err := validateRequestInfo(info); err != nil {
		return nil, err
	}
	err := connect.NewError(connect.Code(request.GetCode()), errorMessage)
	if p.includeErrorDetails {
		detail, detailErr := connectproto.NewErrorDetail(&pingv1.FailRequest{Code: request.GetCode()})
		if detailErr != nil {
			return nil, connect.NewError(connect.CodeInternal, detailErr.Error())
		}
		err = err.WithDetail(detail)
	}
	copyClientHeaderToResponse(info)
	return nil, err
}

func (p pingServer) Sum(ctx context.Context, stream pingv1connect.PingServiceSumServerStream) (*pingv1.SumResponse, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	if err := validateRequestInfo(info); err != nil {
		return nil, err
	}
	if p.checkMetadata {
		if err := expectMetadata(info.RequestHeader()); err != nil {
			return nil, err
		}
	}
	var sum int64
	for {
		msg, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		sum += msg.GetNumber()
	}
	copyClientHeaderToResponse(info)
	return &pingv1.SumResponse{Sum: sum}, nil
}

func (p pingServer) CountUp(ctx context.Context, request *pingv1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	if err := validateRequestInfo(info); err != nil {
		return err
	}
	if p.checkMetadata {
		if err := expectMetadata(info.RequestHeader()); err != nil {
			return err
		}
	}
	if request.GetNumber() <= 0 {
		return connect.Errorf(connect.CodeInvalidArgument, "number must be positive: got %v", request.GetNumber())
	}
	copyClientHeaderToResponse(info)
	for i := range request.GetNumber() {
		if err := stream.Send(&pingv1.CountUpResponse{Number: i + 1}); err != nil {
			return err
		}
	}
	return nil
}

func (p pingServer) CumSum(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	return handleCumSum(ctx, stream, p.checkMetadata)
}

// handleCumSum handles the bidi endpoint CumSum.
func handleCumSum(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream, checkMetadata bool) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	if err := validateRequestInfo(info); err != nil {
		return err
	}
	if checkMetadata {
		if err := expectMetadata(info.RequestHeader()); err != nil {
			return err
		}
	}
	var sum int64
	copyClientHeaderToResponse(info)
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

// copyClientHeaderToResponse copies the client request header values to the
// response headers and trailers so the client can verify propagation.
func copyClientHeaderToResponse(info *connect.CallInfo) {
	for _, el := range info.RequestHeader().Values(clientHeader) {
		info.ResponseHeader().Add(handlerHeader, el)
		info.ResponseTrailer().Add(handlerTrailer, el)
	}
}

func validateRequestInfo(callInfo *connect.CallInfo) error {
	if callInfo == nil || callInfo.PeerAddr == "" {
		return connect.NewError(connect.CodeInternal, "no peer address")
	}
	if callInfo.Protocol == "" {
		return connect.NewError(connect.CodeInternal, "no peer protocol")
	}
	return nil
}

// expectMetadata returns an error if meta doesn't contain the expected header
// values. Used with the server's checkMetadata setting to force an error.
func expectMetadata(meta *connect.Header) error {
	vals := meta.Values(clientHeader)
	if !compareValues(vals, expectedHeaderValues) {
		return connect.Errorf(connect.CodeInvalidArgument,
			"header %q: got %q, expected %q",
			clientHeader,
			vals,
			expectedHeaderValues,
		)
	}
	return nil
}

// compareValues compares two string slices of header values, ignoring order.
func compareValues(hdr1 []string, hdr2 []string) bool {
	if len(hdr1) != len(hdr2) {
		return false
	}
	sorted1 := make([]string, len(hdr1))
	copy(sorted1, hdr1)
	sorted2 := make([]string, len(hdr2))
	copy(sorted2, hdr2)
	sort.Strings(sorted1)
	sort.Strings(sorted2)
	for i := range sorted1 {
		if sorted1[i] != sorted2[i] {
			return false
		}
	}
	return true
}

// metaValues returns key's values across response headers and trailers, since
// gRPC errors are trailers-only while Connect keeps the header/trailer split.
func metaValues(callInfo *connect.CallInfo, key string) []string {
	out := append([]string{}, callInfo.ResponseHeader().Values(key)...)
	return append(out, callInfo.ResponseTrailer().Values(key)...)
}

type deflateCompressor struct{}

func (deflateCompressor) Name() string { return "deflate" }

func (deflateCompressor) Compress(dst io.Writer) (io.WriteCloser, error) {
	return flate.NewWriter(dst, flate.DefaultCompression)
}

func (deflateCompressor) Decompress(src io.Reader) (io.ReadCloser, error) {
	return flate.NewReader(src), nil
}

var _ connect.Compressor = deflateCompressor{}

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

type failCompressor struct{}

func (failCompressor) Name() string { return "fail" }

func (failCompressor) Compress(dst io.Writer) (io.WriteCloser, error) {
	return failCompressorWriteCloser{}, nil
}
func (failCompressor) Decompress(src io.Reader) (io.ReadCloser, error) {
	return failCompressorReadCloser{}, nil
}

type failCompressorWriteCloser struct{}

func (failCompressorWriteCloser) Write([]byte) (int, error) {
	return 0, errors.New("failCompressor")
}
func (failCompressorWriteCloser) Close() error {
	return errors.New("failCompressor")
}

type failCompressorReadCloser struct{}

func (failCompressorReadCloser) Read([]byte) (int, error) {
	return 0, errors.New("failCompressor")
}
func (failCompressorReadCloser) Close() error {
	return errors.New("failCompressor")
}

func failNoHTTP2(tb testing.TB, stream pingv1connect.PingServiceCumSumClientStream) {
	tb.Helper()
	if err := stream.Send(&pingv1.CumSumRequest{}); err != nil {
		assert.ErrorIs(tb, err, io.EOF)
		assert.Equal(tb, connect.CodeOf(err), connect.CodeUnknown)
	}
	assert.Nil(tb, stream.CloseSend())
	_, err := stream.Receive()
	assert.NotNil(tb, err) // should be 505
	assert.True(
		tb,
		strings.Contains(err.Error(), "HTTP status 505"),
		assert.Sprintf("expected 505, got %v", err),
	)
	assert.Nil(tb, stream.Close())
}

func testUnary(t *testing.T, client pingv1connect.PingServiceClient) { //nolint:thelper
	num := int64(42)
	ctx, callInfo := connect.NewClientContext(t.Context())
	for _, el := range expectedHeaderValues {
		callInfo.RequestHeader().Add(clientHeader, el)
	}
	expect := &pingv1.PingResponse{Number: num}
	response, err := client.Ping(ctx, &pingv1.PingRequest{Number: num})
	assert.Equal(t, response, expect)
	assert.Nil(t, err)
	assert.Equal(t, callInfo.PeerAddr, httptest.DefaultRemoteAddr)
	// When using the simple API for unary calls, users can only access response headers and trailers
	// from the call info in context.
	assertResponseHeadersAndTrailers(t, callInfo)
}

func testServerStream(t *testing.T, client pingv1connect.PingServiceClient) { //nolint:thelper
	ctx, callInfo := connect.NewClientContext(t.Context())
	for _, el := range expectedHeaderValues {
		callInfo.RequestHeader().Add(clientHeader, el)
	}
	val := 3
	stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{
		Number: int64(val),
	})
	assert.Nil(t, err)
	// Receive expected messages
	for idx := range val {
		expected := int64(idx + 1)
		msg, err := stream.Receive()
		assert.Nil(t, err)
		assert.NotNil(t, msg)
		assert.Equal(t, msg.GetNumber(), expected)
	}

	// Stream should be done. Expect EOF on receive and close stream
	_, err = stream.Receive()
	assert.True(t, errors.Is(err, io.EOF))
	assert.Nil(t, stream.Close())
	assert.Equal(t, callInfo.Spec.StreamType, connect.StreamTypeServer)
	assert.Equal(t, callInfo.Spec.Procedure, pingv1connect.PingServiceCountUpProcedure)
	assert.Equal(t, callInfo.PeerAddr, httptest.DefaultRemoteAddr)

	// On server-streaming calls, users can access response headers and trailers
	// either from the call info in context or from the stream itself.
	// This verifies that the both the stream and the call info have the same information
	assertResponseHeadersAndTrailers(t, callInfo)
}

func testClientStream(t *testing.T, client pingv1connect.PingServiceClient) { //nolint:thelper
	ctx, callInfo := connect.NewClientContext(t.Context())
	for _, el := range expectedHeaderValues {
		callInfo.RequestHeader().Add(clientHeader, el)
	}

	const (
		upTo   = 10
		expect = 55 // 1+10 + 2+9 + ... + 5+6 = 55
	)
	stream, err := client.Sum(ctx)
	assert.Nil(t, err)

	// Send messages
	for i := range upTo {
		err := stream.Send(&pingv1.SumRequest{Number: int64(i + 1)})
		assert.Nil(t, err, assert.Sprintf("send %d", i))
	}

	response, err := stream.CloseAndReceive()
	assert.Nil(t, err)
	assert.Equal(t, response.GetSum(), expect)

	assert.Equal(t, callInfo.Spec.StreamType, connect.StreamTypeClient)
	assert.Equal(t, callInfo.Spec.Procedure, pingv1connect.PingServiceSumProcedure)
	assert.Equal(t, callInfo.PeerAddr, httptest.DefaultRemoteAddr)

	assertResponseHeadersAndTrailers(t, callInfo)
}

func testBidiStream(t *testing.T, client pingv1connect.PingServiceClient, expectSuccess bool) { //nolint:thelper
	send := []int64{3, 5, 1}
	expect := []int64{3, 8, 9}
	var got []int64
	ctx, callInfo := connect.NewClientContext(t.Context())
	callInfo.RequestHeader().Add(clientHeader, "foo")
	callInfo.RequestHeader().Add(clientHeader, "bar")

	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	assert.NotNil(t, stream)

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
		assert.Nil(t, stream.CloseSend())
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
		assert.Nil(t, stream.Close())
	}()
	wg.Wait()
	assert.Equal(t, got, expect)

	assert.Equal(t, callInfo.Spec.StreamType, connect.StreamTypeBidi)
	assert.Equal(t, callInfo.Spec.Procedure, pingv1connect.PingServiceCumSumProcedure)
	assert.Equal(t, callInfo.PeerAddr, httptest.DefaultRemoteAddr)

	assertResponseHeadersAndTrailers(t, callInfo)
}

// assertResponseHeadersAndTrailers verifies that the given response info contains the expected headers and trailers.
func assertResponseHeadersAndTrailers(t *testing.T, callInfo *connect.CallInfo) { //nolint:thelper
	assert.True(t, compareValues(callInfo.ResponseHeader().Values(handlerHeader), expectedHeaderValues))
	assert.True(t, compareValues(callInfo.ResponseTrailer().Values(handlerTrailer), expectedHeaderValues))
}

// assertErrorResponseMetadata verifies the handler's error metadata reached the
// client. gRPC sends a trailers-only error response, folding leading metadata
// into trailers, so handlerHeader may arrive as a header or a trailer.
func assertErrorResponseMetadata(t *testing.T, callInfo *connect.CallInfo) { //nolint:thelper
	header := append([]string{}, callInfo.ResponseHeader().Values(handlerHeader)...)
	header = append(header, callInfo.ResponseTrailer().Values(handlerHeader)...)
	assert.True(t, compareValues(header, expectedHeaderValues))
	assert.True(t, compareValues(callInfo.ResponseTrailer().Values(handlerTrailer), expectedHeaderValues))
}
