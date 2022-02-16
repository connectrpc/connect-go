package connect_test

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bufconnect/connect"
	"github.com/bufconnect/connect/codec/protobuf"
	"github.com/bufconnect/connect/compress/gzip"
	"github.com/bufconnect/connect/handlerstream"
	"github.com/bufconnect/connect/health"
	"github.com/bufconnect/connect/internal/assert"
	pingrpc "github.com/bufconnect/connect/internal/gen/proto/go-connect/connect/ping/v1test"
	pingpb "github.com/bufconnect/connect/internal/gen/proto/go/connect/ping/v1test"
	"github.com/bufconnect/connect/reflection"
)

const errorMessage = "oh no"

type pingServer struct {
	pingrpc.UnimplementedPingServiceHandler
}

func (p pingServer) Ping(ctx context.Context, req *connect.Request[pingpb.PingRequest]) (*connect.Response[pingpb.PingResponse], error) {
	return connect.NewResponse(&pingpb.PingResponse{
		Number: req.Msg.Number,
		Msg:    req.Msg.Msg,
	}), nil
}

func (p pingServer) Fail(ctx context.Context, req *connect.Request[pingpb.FailRequest]) (*connect.Response[pingpb.FailResponse], error) {
	return nil, connect.Errorf(connect.Code(req.Msg.Code), errorMessage)
}

func (p pingServer) Sum(
	ctx context.Context,
	stream *handlerstream.Client[pingpb.SumRequest, pingpb.SumResponse],
) error {
	var sum int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(connect.NewResponse(&pingpb.SumResponse{
				Sum: sum,
			}))
		} else if err != nil {
			return err
		}
		sum += msg.Number
	}
}

func (p pingServer) CountUp(
	ctx context.Context,
	req *connect.Request[pingpb.CountUpRequest],
	stream *handlerstream.Server[pingpb.CountUpResponse],
) error {
	if req.Msg.Number <= 0 {
		return connect.Errorf(connect.CodeInvalidArgument, "number must be positive: got %v", req.Msg.Number)
	}
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
	stream *handlerstream.Bidirectional[pingpb.CumSumRequest, pingpb.CumSumResponse],
) error {
	var sum int64
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
	mux, err := connect.NewServeMux(
		pingrpc.WithPingServiceHandler(pingServer{}, registrar),
		health.WithHandler(health.NewChecker(registrar)),
		reflection.WithHandler(registrar),
	)
	assert.Nil(t, err, "mux construction error")

	testPing := func(t *testing.T, client pingrpc.PingServiceClient) {
		t.Run("ping", func(t *testing.T) {
			num := rand.Int63()
			req := &pingpb.PingRequest{Number: num}
			expect := &pingpb.PingResponse{Number: num}
			res, err := client.Ping(context.Background(), connect.NewRequest(req))
			assert.Nil(t, err, "ping error")
			assert.Equal(t, res.Msg, expect, "ping response")
		})
		t.Run("large ping", func(t *testing.T) {
			// Using a large payload splits the request and response over multiple
			// packets, ensuring that we're managing HTTP readers and writers
			// correctly.
			hellos := strings.Repeat("hello", 1024*1024) // ~5mb
			req := connect.NewRequest(&pingpb.PingRequest{Msg: hellos})
			res, err := client.Ping(context.Background(), req)
			assert.Nil(t, err, "ping error")
			assert.Equal(t, res.Msg.Msg, hellos, "ping response")
		})
	}
	testSum := func(t *testing.T, client pingrpc.PingServiceClient) {
		t.Run("sum", func(t *testing.T) {
			const (
				upTo   = 10
				expect = 55 // 1+10 + 2+9 + ... + 5+6 = 55
			)
			stream := client.Sum(context.Background())
			for i := int64(1); i <= upTo; i++ {
				err := stream.Send(&pingpb.SumRequest{Number: i})
				assert.Nil(t, err, "Send %v", assert.Fmt(i))
			}
			res, err := stream.CloseAndReceive()
			assert.Nil(t, err, "CloseAndReceive error")
			assert.Equal(t, res.Msg.Sum, expect, "response sum")
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
			stream, err := client.CountUp(
				context.Background(),
				connect.NewRequest(&pingpb.CountUpRequest{Number: n}),
			)
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
		})
	}
	testErrors := func(t *testing.T, client pingrpc.PingServiceClient) {
		t.Run("errors", func(t *testing.T) {
			request := connect.NewRequest(&pingpb.FailRequest{
				Code: int32(connect.CodeResourceExhausted),
			})
			response, err := client.Fail(context.Background(), request)
			assert.Nil(t, response, "fail RPC response")
			assert.NotNil(t, err, "fail RPC error")
			connectErr, ok := connect.AsError(err)
			assert.True(t, ok, "conversion to *connect.Error")
			assert.Equal(t, connectErr.Code(), connect.CodeResourceExhausted, "error code")
			assert.Equal(t, connectErr.Error(), "ResourceExhausted: "+errorMessage, "error message")
			assert.Zero(t, connectErr.Details(), "error details")
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
			run(t, connect.WithRequestCompressor(gzip.Name))
		})
		t.Run("json_gzip", func(t *testing.T) {
			run(
				t,
				connect.WithCodec(protobuf.NameJSON, protobuf.NewJSON()),
				connect.WithRequestCompressor(gzip.Name),
			)
		})
		t.Run("web", func(t *testing.T) {
			run(t, connect.WithGRPCWeb(true))
		})
		t.Run("web_json_gzip", func(t *testing.T) {
			run(
				t,
				connect.WithGRPCWeb(true),
				connect.WithCodec(protobuf.NameJSON, protobuf.NewJSON()),
				connect.WithRequestCompressor(gzip.Name),
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

	ping func(context.Context, *connect.Request[pingpb.PingRequest]) (*connect.Response[pingpb.PingResponse], error)
}

func (p *pluggablePingServer) Ping(
	ctx context.Context,
	req *connect.Request[pingpb.PingRequest],
) (*connect.Response[pingpb.PingResponse], error) {
	return p.ping(ctx, req)
}

func TestHeaderBasic(t *testing.T) {
	const (
		key  = "Test-Key"
		cval = "client value"
		hval = "client value"
	)

	pingServer := &pluggablePingServer{
		ping: func(ctx context.Context, req *connect.Request[pingpb.PingRequest]) (*connect.Response[pingpb.PingResponse], error) {
			assert.Equal(t, req.Header().Get(key), cval, "expected handler to receive headers")
			res := connect.NewResponse(&pingpb.PingResponse{})
			res.Header().Set(key, hval)
			return res, nil
		},
	}
	mux, err := connect.NewServeMux(pingrpc.WithPingServiceHandler(pingServer))
	assert.Nil(t, err, "mux construction error")
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := pingrpc.NewPingServiceClient(server.URL, server.Client())
	assert.Nil(t, err, "client construction error")
	req := connect.NewRequest(&pingpb.PingRequest{})
	req.Header().Set(key, cval)
	res, err := client.Ping(context.Background(), req)
	assert.Nil(t, err, "error making request")
	assert.Equal(t, res.Header().Get(key), hval, "expected client to receive headers")
}
