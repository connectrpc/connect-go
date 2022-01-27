package rerpc_test

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rerpc/rerpc"
	"github.com/rerpc/rerpc/codec/protobuf"
	"github.com/rerpc/rerpc/compress"
	"github.com/rerpc/rerpc/handlerstream"
	"github.com/rerpc/rerpc/health"
	"github.com/rerpc/rerpc/internal/assert"
	pingrpc "github.com/rerpc/rerpc/internal/gen/proto/go-rerpc/rerpc/ping/v1test"
	pingpb "github.com/rerpc/rerpc/internal/gen/proto/go/rerpc/ping/v1test"
	"github.com/rerpc/rerpc/reflection"
)

const errMsg = "oh no"

type pingServer struct {
	pingrpc.UnimplementedPingServiceServer
}

func (p pingServer) Ping(ctx context.Context, req *pingpb.PingRequest) (*pingpb.PingResponse, error) {
	return &pingpb.PingResponse{
		Number: req.Number,
		Msg:    req.Msg,
	}, nil
}

func (p pingServer) Fail(ctx context.Context, req *pingpb.FailRequest) (*pingpb.FailResponse, error) {
	return nil, rerpc.Errorf(rerpc.Code(req.Code), errMsg)
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
			return stream.SendAndClose(rerpc.NewResponse(&pingpb.SumResponse{
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
	req *pingpb.CountUpRequest,
	stream *handlerstream.Server[pingpb.CountUpResponse],
) error {
	if req.Number <= 0 {
		return rerpc.Errorf(rerpc.CodeInvalidArgument, "number must be positive: got %v", req.Number)
	}
	for i := int64(1); i <= req.Number; i++ {
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
	const errMsg = "oh no"
	reg := rerpc.NewRegistrar()
	mux, err := rerpc.NewServeMux(
		rerpc.NewNotFoundHandler(),
		pingrpc.NewPingService(pingServer{}, reg),
		health.NewService(health.NewChecker(reg)),
		reflection.NewService(reg),
	)
	assert.Nil(t, err, "mux construction error")

	testPing := func(t *testing.T, client pingrpc.SimplePingServiceClient) {
		t.Run("ping", func(t *testing.T) {
			num := rand.Int63()
			req := pingpb.PingRequest{Number: num}
			expect := pingpb.PingResponse{Number: num}
			res, err := client.Ping(context.Background(), &req)
			assert.Nil(t, err, "ping error")
			assert.Equal(t, res, &expect, "ping response")
		})
		t.Run("large ping", func(t *testing.T) {
			// Using a large payload splits the request and response over multiple
			// packets, ensuring that we're managing HTTP readers and writers
			// correctly.
			hellos := strings.Repeat("hello", 1024*1024) // ~5mb
			req := pingpb.PingRequest{Msg: hellos}
			res, err := client.Ping(context.Background(), &req)
			assert.Nil(t, err, "ping error")
			assert.Equal(t, res.Msg, hellos, "ping response")
		})
	}
	testSum := func(t *testing.T, client pingrpc.SimplePingServiceClient) {
		t.Run("sum", func(t *testing.T) {
			const upTo = 10
			const expect = 55 // 1+10 + 2+9 + ... + 5+6 = 55
			stream := client.Sum(context.Background())
			for i := int64(1); i <= upTo; i++ {
				err := stream.Send(&pingpb.SumRequest{Number: i})
				assert.Nil(t, err, "Send %v", assert.Fmt(i))
			}
			res, err := stream.CloseAndReceive()
			assert.Nil(t, err, "CloseAndReceive error")
			assert.Equal(t, res.Msg, &pingpb.SumResponse{Sum: expect}, "response")
		})
	}
	testCountUp := func(t *testing.T, client pingrpc.SimplePingServiceClient) {
		t.Run("count_up", func(t *testing.T) {
			const n = 5
			got := make([]int64, 0, n)
			expect := make([]int64, 0, n)
			for i := 1; i <= n; i++ {
				expect = append(expect, int64(i))
			}
			stream, err := client.CountUp(
				context.Background(),
				&pingpb.CountUpRequest{Number: n},
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
	testCumSum := func(t *testing.T, client pingrpc.SimplePingServiceClient, expectSuccess bool) {
		t.Run("cumsum", func(t *testing.T) {
			send := []int64{3, 5, 1}
			expect := []int64{3, 8, 9}
			var got []int64
			stream := client.CumSum(context.Background())
			if !expectSuccess {
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
					assert.Nil(t, err, "send error #%v", assert.Fmt(i))
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
	testErrors := func(t *testing.T, client pingrpc.SimplePingServiceClient) {
		t.Run("errors", func(t *testing.T) {
			req := pingpb.FailRequest{Code: int32(rerpc.CodeResourceExhausted)}
			res, err := client.Fail(context.Background(), &req)
			assert.Nil(t, res, "fail RPC response")
			assert.NotNil(t, err, "fail RPC error")
			rerr, ok := rerpc.AsError(err)
			assert.True(t, ok, "conversion to *rerpc.Error")
			assert.Equal(t, rerr.Code(), rerpc.CodeResourceExhausted, "error code")
			assert.Equal(t, rerr.Error(), "ResourceExhausted: "+errMsg, "error message")
			assert.Zero(t, rerr.Details(), "error details")
		})
	}
	testMatrix := func(t *testing.T, server *httptest.Server, bidi bool) {
		t.Run("identity", func(t *testing.T) {
			client, err := pingrpc.NewPingServiceClient(server.URL, server.Client())
			assert.Nil(t, err, "client construction error")
			testPing(t, client)
			testSum(t, client)
			testCountUp(t, client)
			testCumSum(t, client, bidi)
			testErrors(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			client, err := pingrpc.NewPingServiceClient(
				server.URL,
				server.Client(),
				rerpc.UseCompressor(compress.NameGzip),
			)
			assert.Nil(t, err, "client construction error")
			testPing(t, client)
			testSum(t, client)
			testCountUp(t, client)
			testCumSum(t, client, bidi)
			testErrors(t, client)
		})
		t.Run("json_gzip", func(t *testing.T) {
			client, err := pingrpc.NewPingServiceClient(
				server.URL,
				server.Client(),
				rerpc.Codec(protobuf.NameJSON, protobuf.NewJSON()),
				rerpc.UseCompressor(compress.NameGzip),
			)
			assert.Nil(t, err, "client construction error")
			testPing(t, client)
			testSum(t, client)
			testCountUp(t, client)
			testCumSum(t, client, bidi)
			testErrors(t, client)
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
	pingrpc.UnimplementedPingServiceServer

	ping func(context.Context, *rerpc.Request[pingpb.PingRequest]) (*rerpc.Response[pingpb.PingResponse], error)
}

func (p *pluggablePingServer) Ping(ctx context.Context, req *rerpc.Request[pingpb.PingRequest]) (*rerpc.Response[pingpb.PingResponse], error) {
	return p.ping(ctx, req)
}

func TestHeaderBasic(t *testing.T) {
	const key = "Test-Key"
	const cval, hval = "client value", "handler value"

	srv := &pluggablePingServer{
		ping: func(ctx context.Context, req *rerpc.Request[pingpb.PingRequest]) (*rerpc.Response[pingpb.PingResponse], error) {
			assert.Equal(t, req.Header().Get(key), cval, "expected handler to receive headers")
			res := rerpc.NewResponse(&pingpb.PingResponse{})
			res.Header().Set(key, hval)
			return res, nil
		},
	}
	mux, err := rerpc.NewServeMux(
		rerpc.NewNotFoundHandler(),
		pingrpc.NewFullPingService(srv),
	)
	assert.Nil(t, err, "mux construction error")
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := pingrpc.NewPingServiceClient(server.URL, server.Client())
	assert.Nil(t, err, "client construction error")
	req := rerpc.NewRequest(&pingpb.PingRequest{})
	req.Header().Set(key, cval)
	res, err := client.Full().Ping(context.Background(), req)
	assert.Nil(t, err, "error making request")
	assert.Equal(t, res.Header().Get(key), hval, "expected client to receive headers")
}
