package connect_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bufbuild/connect"
	"github.com/bufbuild/connect/health"
	"github.com/bufbuild/connect/internal/assert"
	pingrpc "github.com/bufbuild/connect/internal/gen/proto/go-connect/connect/ping/v1test"
	pingpb "github.com/bufbuild/connect/internal/gen/proto/go/connect/ping/v1test"
	"github.com/bufbuild/connect/reflection"
)

const errorMessage = "oh no"

const (
	headerValue    = "some header value"
	trailerValue   = "some trailer value"
	clientHeader   = "Connect-Client-Header"
	clientTrailer  = "Connect-Client-Trailer"
	handlerHeader  = "Connect-Handler-Header"
	handlerTrailer = "Connect-Handler-Trailer"
)

func expectClientHeaderAndTrailer(check bool, req connect.AnyEnvelope) error {
	if !check {
		return nil
	}
	if err := expectMetadata(req.Header(), "header", clientHeader, headerValue); err != nil {
		return err
	}
	if err := expectMetadata(req.Trailer(), "trailer", clientTrailer, trailerValue); err != nil {
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
	pingrpc.UnimplementedPingServiceHandler

	checkMetadata bool
}

func (p pingServer) Ping(ctx context.Context, req *connect.Envelope[pingpb.PingRequest]) (*connect.Envelope[pingpb.PingResponse], error) {
	if err := expectClientHeaderAndTrailer(p.checkMetadata, req); err != nil {
		return nil, err
	}
	res := connect.NewEnvelope(&pingpb.PingResponse{
		Number: req.Msg.Number,
		Text:   req.Msg.Text,
	})
	res.Header().Set(handlerHeader, headerValue)
	res.Trailer().Set(handlerTrailer, trailerValue)
	return res, nil
}

func (p pingServer) Fail(ctx context.Context, req *connect.Envelope[pingpb.FailRequest]) (*connect.Envelope[pingpb.FailResponse], error) {
	if err := expectClientHeaderAndTrailer(p.checkMetadata, req); err != nil {
		return nil, err
	}
	err := connect.NewError(connect.Code(req.Msg.Code), errors.New(errorMessage))
	err.Header().Set(handlerHeader, headerValue)
	err.Trailer().Set(handlerTrailer, trailerValue)
	return nil, err
}

func (p pingServer) Sum(
	ctx context.Context,
	stream *connect.ClientStream[pingpb.SumRequest, pingpb.SumResponse],
) error {
	if p.checkMetadata {
		if err := expectMetadata(stream.RequestHeader(), "header", clientHeader, headerValue); err != nil {
			return err
		}
	}
	var sum int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			response := connect.NewEnvelope(&pingpb.SumResponse{Sum: sum})
			response.Header().Set(handlerHeader, headerValue)
			response.Trailer().Set(handlerTrailer, trailerValue)
			return stream.SendAndClose(response)
		} else if err != nil {
			return err
		}
		sum += msg.Number
	}
}

func (p pingServer) CountUp(
	ctx context.Context,
	req *connect.Envelope[pingpb.CountUpRequest],
	stream *connect.ServerStream[pingpb.CountUpResponse],
) error {
	if err := expectClientHeaderAndTrailer(p.checkMetadata, req); err != nil {
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
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := stream.Send(&pingpb.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
	return nil
}

func (p pingServer) CumSum(
	ctx context.Context,
	stream *connect.BidiStream[pingpb.CumSumRequest, pingpb.CumSumResponse],
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
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		sum += msg.Number
		if err := stream.Send(&pingpb.CumSumResponse{Sum: sum}); err != nil {
			return err
		}
	}
}

func TestServerProtoGRPC(t *testing.T) {
	registrar := connect.NewRegistrar()
	mux := http.NewServeMux()
	mux.Handle(pingrpc.NewPingServiceHandler(
		pingServer{checkMetadata: true},
		connect.WithRegistrar(registrar),
	))
	mux.Handle(health.NewHandler(health.NewChecker(registrar)))
	mux.Handle(reflection.NewHandler(registrar))

	testPing := func(t *testing.T, client pingrpc.PingServiceClient) {
		t.Run("ping", func(t *testing.T) {
			num := rand.Int63()
			req := connect.NewEnvelope(&pingpb.PingRequest{Number: num})
			req.Header().Set(clientHeader, headerValue)
			req.Trailer().Set(clientTrailer, trailerValue)
			expect := &pingpb.PingResponse{Number: num}
			res, err := client.Ping(context.Background(), req)
			assert.Nil(t, err, "ping error")
			assert.Equal(t, res.Msg, expect, "ping response")
			assert.Equal(t, res.Header().Get(handlerHeader), headerValue, "ping header")
			assert.Equal(t, res.Trailer().Get(handlerTrailer), trailerValue, "ping trailer")
		})
		t.Run("large ping", func(t *testing.T) {
			// Using a large payload splits the request and response over multiple
			// packets, ensuring that we're managing HTTP readers and writers
			// correctly.
			hellos := strings.Repeat("hello", 1024*1024) // ~5mb
			req := connect.NewEnvelope(&pingpb.PingRequest{Text: hellos})
			req.Header().Set(clientHeader, headerValue)
			req.Trailer().Set(clientTrailer, trailerValue)
			res, err := client.Ping(context.Background(), req)
			assert.Nil(t, err, "ping error")
			assert.Equal(t, res.Msg.Text, hellos, "ping response")
			assert.Equal(t, res.Header().Get(handlerHeader), headerValue, "ping header")
			assert.Equal(t, res.Trailer().Get(handlerTrailer), trailerValue, "ping trailer")
		})
	}
	testSum := func(t *testing.T, client pingrpc.PingServiceClient) {
		t.Run("sum", func(t *testing.T) {
			const (
				upTo   = 10
				expect = 55 // 1+10 + 2+9 + ... + 5+6 = 55
			)
			stream := client.Sum(context.Background())
			stream.RequestHeader().Set(clientHeader, headerValue)
			for i := int64(1); i <= upTo; i++ {
				err := stream.Send(&pingpb.SumRequest{Number: i})
				assert.Nil(t, err, "Send %v", assert.Fmt(i))
			}
			res, err := stream.CloseAndReceive()
			assert.Nil(t, err, "CloseAndReceive error")
			assert.Equal(t, res.Msg.Sum, expect, "response sum")
			assert.Equal(t, res.Header().Get(handlerHeader), headerValue, "response header")
			assert.Equal(t, res.Trailer().Get(handlerTrailer), trailerValue, "response trailer")
		})
	}
	testCountUp := func(t *testing.T, client pingrpc.PingServiceClient) {
		t.Run("count_up", func(t *testing.T) {
			const n = 5
			got := make([]int64, 0, n)
			expect := make([]int64, 0, n)
			for i := 1; i <= n; i++ {
				expect = append(expect, int64(i))
			}
			request := connect.NewEnvelope(&pingpb.CountUpRequest{Number: n})
			request.Header().Set(clientHeader, headerValue)
			request.Trailer().Set(clientTrailer, trailerValue)
			stream, err := client.CountUp(context.Background(), request)
			assert.Nil(t, err, "send error")
			for {
				msg, err := stream.Receive()
				if errors.Is(err, io.EOF) {
					break
				}
				assert.Nil(t, err, "receive error")
				got = append(got, msg.Number)
			}
			err = stream.Close()
			assert.Nil(t, err, "close error")
			assert.Equal(t, got, expect, "responses")
		})
	}
	testCumSum := func(t *testing.T, client pingrpc.PingServiceClient, expectSuccess bool) {
		t.Run("cumsum", func(t *testing.T) {
			send := []int64{3, 5, 1}
			expect := []int64{3, 8, 9}
			var got []int64
			stream := client.CumSum(context.Background())
			stream.RequestHeader().Set(clientHeader, headerValue)
			if !expectSuccess { // server doesn't support HTTP/2
				err := stream.Send(&pingpb.CumSumRequest{})
				assert.Nil(t, err, "first send on HTTP/1.1") // succeeds, haven't gotten response back yet
				assert.Nil(t, stream.CloseSend(), "close send error on HTTP/1.1")
				_, err = stream.Receive()
				assert.NotNil(t, err, "first receive on HTTP/1.1") // should be 505
				assert.True(t, strings.Contains(err.Error(), "HTTP status 505"), "expected 505, got %v", assert.Fmt(err))
				assert.Nil(t, stream.CloseReceive(), "close receive error on HTTP/1.1")
				return
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for i, n := range send {
					err := stream.Send(&pingpb.CumSumRequest{Number: n})
					assert.Nil(t, err, "send error #%d", assert.Fmt(i))
				}
				assert.Nil(t, stream.CloseSend(), "close send error")
			}()
			go func() {
				defer wg.Done()
				for {
					msg, err := stream.Receive()
					if errors.Is(err, io.EOF) {
						break
					}
					assert.Nil(t, err, "receive error")
					got = append(got, msg.Sum)
				}
				assert.Nil(t, stream.CloseReceive(), "close receive error")
			}()
			wg.Wait()
			assert.Equal(t, got, expect, "sums")
			assert.Equal(
				t,
				stream.ResponseHeader().Get(handlerHeader),
				headerValue,
				"custom header",
			)
			assert.Equal(
				t,
				stream.ResponseTrailer().Get(handlerTrailer),
				trailerValue,
				"custom header",
			)
		})
	}
	testErrors := func(t *testing.T, client pingrpc.PingServiceClient) {
		t.Run("errors", func(t *testing.T) {
			request := connect.NewEnvelope(&pingpb.FailRequest{
				Code: int32(connect.CodeResourceExhausted),
			})
			request.Header().Set(clientHeader, headerValue)
			request.Trailer().Set(clientTrailer, trailerValue)

			response, err := client.Fail(context.Background(), request)
			assert.Nil(t, response, "fail RPC response")
			assert.NotNil(t, err, "fail RPC error")
			var connectErr *connect.Error
			ok := errors.As(err, &connectErr)
			assert.True(t, ok, "conversion to *connect.Error")
			assert.Equal(t, connectErr.Code(), connect.CodeResourceExhausted, "error code")
			assert.Equal(t, connectErr.Error(), "ResourceExhausted: "+errorMessage, "error message")
			assert.Zero(t, connectErr.Details(), "error details")
			assert.Equal(
				t,
				connectErr.Header().Get(handlerHeader),
				headerValue,
				"custom header",
			)
			assert.Equal(
				t,
				connectErr.Trailer().Get(handlerTrailer),
				trailerValue,
				"custom trailer",
			)
		})
	}
	testMatrix := func(t *testing.T, server *httptest.Server, bidi bool) {
		run := func(t *testing.T, opts ...connect.ClientOption) {
			client, err := pingrpc.NewPingServiceClient(server.URL, server.Client(), opts...)
			assert.Nil(t, err, "client construction error")
			testPing(t, client)
			testSum(t, client)
			testCountUp(t, client)
			testCumSum(t, client, bidi)
			testErrors(t, client)
		}
		t.Run("identity", func(t *testing.T) {
			run(t)
		})
		t.Run("gzip", func(t *testing.T) {
			run(t, connect.WithGzipRequests())
		})
		t.Run("json_gzip", func(t *testing.T) {
			run(
				t,
				connect.WithProtobufJSONCodec(),
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
				connect.WithProtobufJSONCodec(),
				connect.WithGzipRequests(),
			)
		})
	}

	t.Run("http1", func(t *testing.T) {
		server := httptest.NewServer(mux)
		defer server.Close()
		testMatrix(t, server, false /* bidi */)
	})
	t.Run("http2", func(t *testing.T) {
		server := httptest.NewUnstartedServer(mux)
		server.EnableHTTP2 = true
		server.StartTLS()
		defer server.Close()
		testMatrix(t, server, true /* bidi */)
	})
}

type pluggablePingServer struct {
	pingrpc.UnimplementedPingServiceHandler

	ping func(context.Context, *connect.Envelope[pingpb.PingRequest]) (*connect.Envelope[pingpb.PingResponse], error)
}

func (p *pluggablePingServer) Ping(
	ctx context.Context,
	req *connect.Envelope[pingpb.PingRequest],
) (*connect.Envelope[pingpb.PingResponse], error) {
	return p.ping(ctx, req)
}

func TestHeaderBasic(t *testing.T) {
	const (
		key  = "Test-Key"
		cval = "client value"
		hval = "client value"
	)

	pingServer := &pluggablePingServer{
		ping: func(ctx context.Context, req *connect.Envelope[pingpb.PingRequest]) (*connect.Envelope[pingpb.PingResponse], error) {
			assert.Equal(t, req.Header().Get(key), cval, "expected handler to receive headers")
			res := connect.NewEnvelope(&pingpb.PingResponse{})
			res.Header().Set(key, hval)
			return res, nil
		},
	}
	mux := http.NewServeMux()
	mux.Handle(pingrpc.NewPingServiceHandler(pingServer))
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := pingrpc.NewPingServiceClient(server.URL, server.Client())
	assert.Nil(t, err, "client construction error")
	req := connect.NewEnvelope(&pingpb.PingRequest{})
	req.Header().Set(key, cval)
	res, err := client.Ping(context.Background(), req)
	assert.Nil(t, err, "error making request")
	assert.Equal(t, res.Header().Get(key), hval, "expected client to receive headers")
}
