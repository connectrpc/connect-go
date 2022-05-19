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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/bufbuild/connect-go/internal/assert"
	"github.com/bufbuild/connect-go/internal/gen/connect/connect/ping/v1/pingv1connect"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/go/connect/ping/v1"
	"google.golang.org/protobuf/proto"
)

const errorMessage = "oh no"

// The ping server implementation used in the tests returns errors if the
// client doesn't set a header, and the server sets headers and trailers on the
// response.
const (
	headerValue    = "some header value"
	trailerValue   = "some trailer value"
	clientHeader   = "Connect-Client-Header"
	handlerHeader  = "Connect-Handler-Header"
	handlerTrailer = "Connect-Handler-Trailer"
)

func TestServer(t *testing.T) {
	t.Parallel()
	testPing := func(t *testing.T, client pingv1connect.PingServiceClient) { // nolint:thelper
		t.Run("ping", func(t *testing.T) {
			num := int64(42)
			request := connect.NewRequest(&pingv1.PingRequest{Number: num})
			request.Header().Set(clientHeader, headerValue)
			expect := &pingv1.PingResponse{Number: num}
			response, err := client.Ping(context.Background(), request)
			assert.Nil(t, err)
			assert.Equal(t, response.Msg, expect)
			assert.Equal(t, response.Header().Get(handlerHeader), headerValue)
			assert.Equal(t, response.Trailer().Get(handlerTrailer), trailerValue)
		})
		t.Run("zero_ping", func(t *testing.T) {
			request := connect.NewRequest(&pingv1.PingRequest{})
			request.Header().Set(clientHeader, headerValue)
			response, err := client.Ping(context.Background(), request)
			assert.Nil(t, err)
			var expect pingv1.PingResponse
			assert.Equal(t, response.Msg, &expect)
			assert.Equal(t, response.Header().Get(handlerHeader), headerValue)
			assert.Equal(t, response.Trailer().Get(handlerTrailer), trailerValue)
		})
		t.Run("large_ping", func(t *testing.T) {
			// Using a large payload splits the request and response over multiple
			// packets, ensuring that we're managing HTTP readers and writers
			// correctly.
			hellos := strings.Repeat("hello", 1024*1024) // ~5mb
			request := connect.NewRequest(&pingv1.PingRequest{Text: hellos})
			request.Header().Set(clientHeader, headerValue)
			response, err := client.Ping(context.Background(), request)
			assert.Nil(t, err)
			assert.Equal(t, response.Msg.Text, hellos)
			assert.Equal(t, response.Header().Get(handlerHeader), headerValue)
			assert.Equal(t, response.Trailer().Get(handlerTrailer), trailerValue)
		})
		t.Run("ping_error", func(t *testing.T) {
			_, err := client.Ping(
				context.Background(),
				connect.NewRequest(&pingv1.PingRequest{}),
			)
			assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument)
		})
		t.Run("ping_timout", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			defer cancel()
			request := connect.NewRequest(&pingv1.PingRequest{})
			request.Header().Set(clientHeader, headerValue)
			_, err := client.Ping(ctx, request)
			assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded)
		})
	}
	testSum := func(t *testing.T, client pingv1connect.PingServiceClient) { // nolint:thelper
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
			assert.Equal(t, response.Msg.Sum, expect)
			assert.Equal(t, response.Header().Get(handlerHeader), headerValue)
			assert.Equal(t, response.Trailer().Get(handlerTrailer), trailerValue)
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
			assert.Equal(t, got.Header().Get(handlerHeader), headerValue)
		})
	}
	testCountUp := func(t *testing.T, client pingv1connect.PingServiceClient) { // nolint:thelper
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
				got = append(got, stream.Msg().Number)
			}
			assert.Nil(t, stream.Err())
			assert.Nil(t, stream.Close())
			assert.Equal(t, got, expect)
		})
		t.Run("count_up_error", func(t *testing.T) {
			stream, err := client.CountUp(
				context.Background(),
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
		})
	}
	testCumSum := func(t *testing.T, client pingv1connect.PingServiceClient, expectSuccess bool) { // nolint:thelper
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
					got = append(got, msg.Sum)
				}
				assert.Nil(t, stream.CloseReceive())
			}()
			wg.Wait()
			assert.Equal(t, got, expect)
			assert.Equal(t, stream.ResponseHeader().Get(handlerHeader), headerValue)
			assert.Equal(t, stream.ResponseTrailer().Get(handlerTrailer), trailerValue)
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
			assert.Nil(t, stream.CloseSend())
			response, err := stream.Receive()
			assert.Nil(t, response)
			assert.True(t, errors.Is(err, io.EOF))
			assert.Nil(t, stream.CloseReceive()) // clean-up the stream
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
			got = append(got, msg.Sum)
			cancel()
			_, err = stream.Receive()
			assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
			assert.Equal(t, got, expect)
		})
		t.Run("cumsum_cancel_before_send", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			stream := client.CumSum(ctx)
			stream.RequestHeader().Set(clientHeader, headerValue)
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 8}))
			cancel()
			// On a subsequent send, ensure that we are still catching context
			// cancellations.
			err := stream.Send(&pingv1.CumSumRequest{Number: 19})
			assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled, assert.Sprintf("%v", err))
		})
	}
	testErrors := func(t *testing.T, client pingv1connect.PingServiceClient) { // nolint:thelper
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
			assert.Equal(t, connectErr.Code(), connect.CodeResourceExhausted)
			assert.Equal(t, connectErr.Error(), "resource_exhausted: "+errorMessage)
			assert.Zero(t, connectErr.Details())
			assert.Equal(t, connectErr.Meta().Get(handlerHeader), headerValue)
			assert.Equal(t, connectErr.Meta().Get(handlerTrailer), trailerValue)
		})
	}
	testMatrix := func(t *testing.T, server *httptest.Server, bidi bool) { // nolint:thelper
		run := func(t *testing.T, opts ...connect.ClientOption) {
			t.Helper()
			client := pingv1connect.NewPingServiceClient(server.Client(), server.URL, opts...)
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
	mux.Handle(pingv1connect.NewPingServiceHandler(
		pingServer{checkMetadata: true},
	))

	t.Run("http1", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(mux)
		defer server.Close()
		testMatrix(t, server, false /* bidi */)
	})
	t.Run("http2", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewUnstartedServer(mux)
		server.EnableHTTP2 = true
		server.StartTLS()
		defer server.Close()
		testMatrix(t, server, true /* bidi */)
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
		ping: func(ctx context.Context, request *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
			assert.Equal(t, request.Header().Get(key), cval)
			response := connect.NewResponse(&pingv1.PingResponse{})
			response.Header().Set(key, hval)
			return response, nil
		},
	}
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer))
	server := httptest.NewServer(mux)
	defer server.Close()

	client := pingv1connect.NewPingServiceClient(server.Client(), server.URL)
	request := connect.NewRequest(&pingv1.PingRequest{})
	request.Header().Set(key, cval)
	response, err := client.Ping(context.Background(), request)
	assert.Nil(t, err)
	assert.Equal(t, response.Header().Get(key), hval)
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
	server := httptest.NewServer(mux)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client := pingv1connect.NewPingServiceClient(server.Client(), server.URL)
	_, err := client.Ping(ctx, connect.NewRequest(&pingv1.PingRequest{}))
	assert.Nil(t, err)
}

func TestGRPCMarshalStatusError(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(
		pingServer{},
		connect.WithCodec(failCodec{}),
	))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	assertInternalError := func(tb testing.TB, opts ...connect.ClientOption) {
		tb.Helper()
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL, opts...)
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
	server := httptest.NewServer(handler)
	defer server.Close()
	client := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL,
	)
	stream := client.CumSum(context.Background())
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{}))
	assert.Nil(t, stream.CloseSend())
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

	ping func(context.Context, *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error)
}

func (p *pluggablePingServer) Ping(
	ctx context.Context,
	request *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {
	return p.ping(ctx, request)
}

func failNoHTTP2(tb testing.TB, stream *connect.ClientBidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse]) {
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
	assert.Nil(tb, stream.CloseReceive())
}

func expectClientHeader(check bool, req connect.AnyRequest) error {
	if !check {
		return nil
	}
	if err := expectMetadata(req.Header(), "header", clientHeader, headerValue); err != nil {
		return err
	}
	return nil
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
	response := connect.NewResponse(
		&pingv1.PingResponse{
			Number: request.Msg.Number,
			Text:   request.Msg.Text,
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
	err := connect.NewError(connect.Code(request.Msg.Code), errors.New(errorMessage))
	err.Meta().Set(handlerHeader, headerValue)
	err.Meta().Set(handlerTrailer, trailerValue)
	return nil, err
}

func (p pingServer) Sum(
	ctx context.Context,
	stream *connect.ClientStream[pingv1.SumRequest, pingv1.SumResponse],
) error {
	if p.checkMetadata {
		if err := expectMetadata(stream.RequestHeader(), "header", clientHeader, headerValue); err != nil {
			return err
		}
	}
	var sum int64
	for stream.Receive() {
		sum += stream.Msg().Number
	}
	if stream.Err() != nil {
		return stream.Err()
	}
	response := connect.NewResponse(&pingv1.SumResponse{Sum: sum})
	response.Header().Set(handlerHeader, headerValue)
	response.Trailer().Set(handlerTrailer, trailerValue)
	return stream.SendAndClose(response)
}

func (p pingServer) CountUp(
	ctx context.Context,
	request *connect.Request[pingv1.CountUpRequest],
	stream *connect.ServerStream[pingv1.CountUpResponse],
) error {
	if err := expectClientHeader(p.checkMetadata, request); err != nil {
		return err
	}
	if request.Msg.Number <= 0 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"number must be positive: got %v",
			request.Msg.Number,
		))
	}
	stream.ResponseHeader().Set(handlerHeader, headerValue)
	stream.ResponseTrailer().Set(handlerTrailer, trailerValue)
	for i := int64(1); i <= request.Msg.Number; i++ {
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
	stream.ResponseHeader().Set(handlerHeader, headerValue)
	stream.ResponseTrailer().Set(handlerTrailer, trailerValue)
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		sum += msg.Number
		if err := stream.Send(&pingv1.CumSumResponse{Sum: sum}); err != nil {
			return err
		}
	}
}
