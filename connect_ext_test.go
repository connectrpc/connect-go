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

	"github.com/bufbuild/connect-go"
	"github.com/bufbuild/connect-go/internal/assert"
	"github.com/bufbuild/connect-go/internal/gen/connect/connect/ping/v1/pingv1connect"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/go/connect/ping/v1"
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
			req := connect.NewRequest(&pingv1.PingRequest{Number: num})
			req.Header().Set(clientHeader, headerValue)
			expect := &pingv1.PingResponse{Number: num}
			res, err := client.Ping(context.Background(), req)
			assert.Nil(t, err)
			assert.Equal(t, res.Msg, expect)
			assert.Equal(t, res.Header().Get(handlerHeader), headerValue)
			assert.Equal(t, res.Trailer().Get(handlerTrailer), trailerValue)
		})
		t.Run("large ping", func(t *testing.T) {
			// Using a large payload splits the request and response over multiple
			// packets, ensuring that we're managing HTTP readers and writers
			// correctly.
			hellos := strings.Repeat("hello", 1024*1024) // ~5mb
			req := connect.NewRequest(&pingv1.PingRequest{Text: hellos})
			req.Header().Set(clientHeader, headerValue)
			res, err := client.Ping(context.Background(), req)
			assert.Nil(t, err)
			assert.Equal(t, res.Msg.Text, hellos)
			assert.Equal(t, res.Header().Get(handlerHeader), headerValue)
			assert.Equal(t, res.Trailer().Get(handlerTrailer), trailerValue)
		})
		t.Run("ping_error", func(t *testing.T) {
			_, err := client.Ping(
				context.Background(),
				connect.NewRequest(&pingv1.PingRequest{}),
			)
			assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument)
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
			res, err := stream.CloseAndReceive()
			assert.Nil(t, err)
			assert.Equal(t, res.Msg.Sum, expect)
			assert.Equal(t, res.Header().Get(handlerHeader), headerValue)
			assert.Equal(t, res.Trailer().Get(handlerTrailer), trailerValue)
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
			res, err := stream.Receive()
			assert.Nil(t, res)
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
			assert.Equal(t, connectErr.Error(), "ResourceExhausted: "+errorMessage)
			assert.Zero(t, connectErr.Details())
			assert.Equal(t, connectErr.Meta().Get(handlerHeader), headerValue)
			assert.Equal(t, connectErr.Meta().Get(handlerTrailer), trailerValue)
		})
	}
	testMatrix := func(t *testing.T, server *httptest.Server, bidi bool) { // nolint:thelper
		run := func(t *testing.T, opts ...connect.ClientOption) {
			t.Helper()
			client, err := pingv1connect.NewPingServiceClient(server.Client(), server.URL, opts...)
			assert.Nil(t, err)
			testPing(t, client)
			testSum(t, client)
			testCountUp(t, client)
			testCumSum(t, client, bidi)
			testErrors(t, client)
		}
		t.Run("identity", func(t *testing.T) {
			run(t, connect.WithGRPC())
		})
		t.Run("gzip", func(t *testing.T) {
			run(t, connect.WithGRPC(), connect.WithGzipRequests())
		})
		t.Run("json_gzip", func(t *testing.T) {
			run(
				t,
				connect.WithGRPC(),
				connect.WithProtoJSONCodec(),
				connect.WithGzipRequests(),
			)
		})
		t.Run("web", func(t *testing.T) {
			run(t, connect.WithGRPCWeb())
		})
		t.Run("web_json_gzip", func(t *testing.T) {
			run(
				t,
				connect.WithGRPCWeb(),
				connect.WithProtoJSONCodec(),
				connect.WithGzipRequests(),
			)
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
		ping: func(ctx context.Context, req *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
			assert.Equal(t, req.Header().Get(key), cval)
			res := connect.NewResponse(&pingv1.PingResponse{})
			res.Header().Set(key, hval)
			return res, nil
		},
	}
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer))
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := pingv1connect.NewPingServiceClient(server.Client(), server.URL, connect.WithGRPC())
	assert.Nil(t, err)
	req := connect.NewRequest(&pingv1.PingRequest{})
	req.Header().Set(key, cval)
	res, err := client.Ping(context.Background(), req)
	assert.Nil(t, err)
	assert.Equal(t, res.Header().Get(key), hval)
}

func TestStatisticsInterceptor(t *testing.T) {
	t.Parallel()

	handlerMetrics := &metricsInterceptor{}
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(
		pingServer{},
		connect.WithCompressMinBytes(128),
		connect.WithInterceptors(handlerMetrics),
	))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	clientMetrics := &metricsInterceptor{}
	client, err := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL,
		connect.WithGRPC(),
		connect.WithRequestCompression("gzip"),
		connect.WithCompressMinBytes(128),
		connect.WithInterceptors(clientMetrics),
	)
	assert.Nil(t, err)

	t.Run("unary", func(t *testing.T) {
		_, err = client.Ping(
			context.Background(),
			connect.NewRequest(&pingv1.PingRequest{
				Text: strings.Repeat("compressible", 128),
			}),
		)
		assert.Nil(t, err)

		assertCorrect := func(tb testing.TB, stats connect.Statistics) {
			tb.Helper()
			t.Logf("%+v", stats)
			assert.Equal(tb, stats.Messages, 1)
			assert.True(tb, stats.WireSize > 0)
			assert.True(tb, stats.UncompressedSize > 0)
			assert.True(tb, stats.Latency > 0)
			assert.True(tb, stats.UncompressedSize > stats.WireSize)
		}
		t.Run("handler sent", func(t *testing.T) {
			t.Parallel()
			assertCorrect(t, handlerMetrics.UnarySent)
		})
		t.Run("handler received", func(t *testing.T) {
			t.Parallel()
			assertCorrect(t, handlerMetrics.UnaryReceived)
		})
		t.Run("client sent", func(t *testing.T) {
			t.Parallel()
			assertCorrect(t, clientMetrics.UnarySent)
		})
		t.Run("client received", func(t *testing.T) {
			t.Parallel()
			assertCorrect(t, clientMetrics.UnaryReceived)
		})
	})
	t.Run("bidi", func(t *testing.T) {
		stream := client.CumSum(context.Background())
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				err := stream.Send(&pingv1.CumSumRequest{Number: 42})
				assert.Nil(t, err)
			}
			assert.Nil(t, stream.CloseSend())
		}()
		go func() {
			defer wg.Done()
			for {
				_, err := stream.Receive()
				if errors.Is(err, io.EOF) {
					break
				}
				assert.Nil(t, err)
			}
			assert.Nil(t, stream.CloseReceive())
		}()
		wg.Wait()

		assertCorrect := func(tb testing.TB, stats connect.Statistics) {
			tb.Helper()
			assert.Equal(tb, handlerMetrics.StreamSent.Messages, 10)
			assert.True(tb, handlerMetrics.StreamSent.WireSize > 0)
			assert.True(tb, handlerMetrics.StreamSent.UncompressedSize > 0)
			assert.True(tb, handlerMetrics.StreamSent.Latency > 0)
			// These messages are too small to compress.
			assert.Equal(tb, handlerMetrics.StreamSent.UncompressedSize, handlerMetrics.StreamSent.WireSize)
		}
		t.Run("handler sent", func(t *testing.T) {
			t.Parallel()
			assertCorrect(t, handlerMetrics.StreamSent)
		})
		t.Run("handler received", func(t *testing.T) {
			t.Parallel()
			assertCorrect(t, handlerMetrics.StreamReceived)
		})
		t.Run("client sent", func(t *testing.T) {
			t.Parallel()
			assertCorrect(t, clientMetrics.StreamSent)
		})
		t.Run("client received", func(t *testing.T) {
			t.Parallel()
			assertCorrect(t, clientMetrics.StreamReceived)
		})
	})
}

type pluggablePingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	ping func(context.Context, *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error)
}

func (p *pluggablePingServer) Ping(
	ctx context.Context,
	req *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {
	return p.ping(ctx, req)
}

func failNoHTTP2(tb testing.TB, stream *connect.BidiStreamForClient[pingv1.CumSumRequest, pingv1.CumSumResponse]) {
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

func (p pingServer) Ping(ctx context.Context, req *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
	if err := expectClientHeader(p.checkMetadata, req); err != nil {
		return nil, err
	}
	res := connect.NewResponse(&pingv1.PingResponse{
		Number: req.Msg.Number,
		Text:   req.Msg.Text,
	})
	res.Header().Set(handlerHeader, headerValue)
	res.Trailer().Set(handlerTrailer, trailerValue)
	return res, nil
}

func (p pingServer) Fail(ctx context.Context, req *connect.Request[pingv1.FailRequest]) (*connect.Response[pingv1.FailResponse], error) {
	if err := expectClientHeader(p.checkMetadata, req); err != nil {
		return nil, err
	}
	err := connect.NewError(connect.Code(req.Msg.Code), errors.New(errorMessage))
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
	req *connect.Request[pingv1.CountUpRequest],
	stream *connect.ServerStream[pingv1.CountUpResponse],
) error {
	if err := expectClientHeader(p.checkMetadata, req); err != nil {
		return err
	}
	if req.Msg.Number <= 0 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"number must be positive: got %v",
			req.Msg.Number,
		))
	}
	stream.ResponseHeader().Set(handlerHeader, headerValue)
	stream.ResponseTrailer().Set(handlerTrailer, trailerValue)
	for i := int64(1); i <= req.Msg.Number; i++ {
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

type metricsInterceptor struct {
	UnarySent, UnaryReceived   connect.Statistics
	StreamSent, StreamReceived connect.Statistics
}

func (i *metricsInterceptor) WrapUnary(unary connect.UnaryStream) connect.UnaryStream {
	return &metricsUnaryStream{
		UnaryStream: unary,
		sent:        &i.UnarySent,
		received:    &i.UnaryReceived,
	}
}

func (i *metricsInterceptor) WrapStreamContext(ctx context.Context) context.Context {
	return ctx
}

func (i *metricsInterceptor) WrapStreamSender(_ context.Context, sender connect.Sender) connect.Sender {
	return &metricsSender{
		Sender: sender,
		sent:   &i.StreamSent,
	}
}

func (i *metricsInterceptor) WrapStreamReceiver(_ context.Context, receiver connect.Receiver) connect.Receiver {
	return &metricsReceiver{
		Receiver: receiver,
		received: &i.StreamReceived,
	}
}

type metricsUnaryStream struct {
	connect.UnaryStream

	sent, received *connect.Statistics
}

func (s *metricsUnaryStream) Close() {
	s.UnaryStream.Close()
	*s.sent, *s.received = s.UnaryStream.Stats()
}

type metricsSender struct {
	connect.Sender

	sent *connect.Statistics
}

func (s *metricsSender) Close(err error) error {
	closeErr := s.Sender.Close(err)
	*s.sent = s.Sender.Stats()
	return closeErr
}

type metricsReceiver struct {
	connect.Receiver

	received *connect.Statistics
}

func (r *metricsReceiver) Close() error {
	closeErr := r.Receiver.Close()
	*r.received = r.Receiver.Stats()
	return closeErr
}
