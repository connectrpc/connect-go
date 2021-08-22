package crosstest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fullstorydev/grpcurl"
	jsonpbv1 "github.com/golang/protobuf/jsonpb"
	protov1 "github.com/golang/protobuf/proto"
	"github.com/jhump/protoreflect/grpcreflect"
	"github.com/twitchtv/twirp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials" // register gzip compressor
	"google.golang.org/grpc/encoding/gzip"
	grpb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/rerpc/rerpc"
	"github.com/rerpc/rerpc/internal/assert"
	crosspb "github.com/rerpc/rerpc/internal/crosstest/v1test"
	"github.com/rerpc/rerpc/reflection"
)

const errMsg = "soirée 🎉" // readable non-ASCII

func newClientH2C() *http.Client {
	// This is wildly insecure - don't do this in production!
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLS: func(netw, addr string, cfg *tls.Config) (net.Conn, error) {
				return net.Dial(netw, addr)
			},
		},
	}
}

type crossServerReRPC struct {
	crosspb.UnimplementedCrossServiceReRPC
}

func (c crossServerReRPC) Ping(ctx context.Context, req *crosspb.PingRequest) (*crosspb.PingResponse, error) {
	if err := req.Sleep.CheckValid(); req.Sleep != nil && err != nil {
		return nil, rerpc.Wrap(rerpc.CodeInvalidArgument, err)
	}
	if d := req.Sleep.AsDuration(); d > 0 {
		time.Sleep(d)
	}
	return &crosspb.PingResponse{Number: req.Number}, nil
}

func (c crossServerReRPC) Fail(ctx context.Context, req *crosspb.FailRequest) (*crosspb.FailResponse, error) {
	return nil, rerpc.Errorf(rerpc.CodeResourceExhausted, errMsg)
}

func (c crossServerReRPC) Sum(ctx context.Context, stream *crosspb.CrossServiceReRPC_Sum) error {
	var sum int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&crosspb.SumResponse{
				Sum: sum,
			})
		} else if err != nil {
			return err
		}
		sum += msg.Number
	}
}

func (c crossServerReRPC) CountUp(ctx context.Context, req *crosspb.CountUpRequest, stream *crosspb.CrossServiceReRPC_CountUp) error {
	if req.Number <= 0 {
		return rerpc.Errorf(rerpc.CodeInvalidArgument, "number must be positive: got %v", req.Number)
	}
	for i := int64(1); i <= req.Number; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := stream.Send(&crosspb.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
	return nil
}

func (c crossServerReRPC) CumSum(ctx context.Context, stream *crosspb.CrossServiceReRPC_CumSum) error {
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
		if err := stream.Send(&crosspb.CumSumResponse{Sum: sum}); err != nil {
			return err
		}
	}
}

type crossServerGRPC struct {
	crosspb.UnimplementedCrossServiceServer
}

func (c crossServerGRPC) Ping(ctx context.Context, req *crosspb.PingRequest) (*crosspb.PingResponse, error) {
	if err := req.Sleep.CheckValid(); req.Sleep != nil && err != nil {
		return nil, grpc.Errorf(codes.InvalidArgument, err.Error())
	}
	if d := req.Sleep.AsDuration(); d > 0 {
		time.Sleep(d)
	}
	return &crosspb.PingResponse{Number: req.Number}, nil
}

func (c crossServerGRPC) Fail(ctx context.Context, req *crosspb.FailRequest) (*crosspb.FailResponse, error) {
	return nil, grpc.Errorf(codes.ResourceExhausted, errMsg)
}

func (c crossServerGRPC) Sum(stream crosspb.CrossService_SumServer) error {
	var sum int64
	for {
		if err := stream.Context().Err(); err != nil {
			return err
		}
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&crosspb.SumResponse{
				Sum: sum,
			})
		} else if err != nil {
			return err
		}
		sum += msg.Number
	}
}

func (c crossServerGRPC) CountUp(req *crosspb.CountUpRequest, stream crosspb.CrossService_CountUpServer) error {
	if req.Number <= 0 {
		return grpc.Errorf(codes.InvalidArgument, "number must be positive: got %v", req.Number)
	}
	for i := int64(1); i <= req.Number; i++ {
		if err := stream.Context().Err(); err != nil {
			return err
		}
		if err := stream.Send(&crosspb.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
	return nil
}

func (c crossServerGRPC) CumSum(stream crosspb.CrossService_CumSumServer) error {
	var sum int64
	for {
		if err := stream.Context().Err(); err != nil {
			return err
		}
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		sum += msg.Number
		if err := stream.Send(&crosspb.CumSumResponse{Sum: sum}); err != nil {
			return err
		}
	}
}

func assertErrorGRPC(t testing.TB, err error, msg string) *status.Status {
	t.Helper()
	assert.NotNil(t, err, msg)
	s, ok := status.FromError(err)
	assert.True(t, ok, "conversion to *status.Status")
	return s
}

func assertErrorReRPC(t testing.TB, err error, msg string) *rerpc.Error {
	t.Helper()
	assert.NotNil(t, err, msg)
	rerr, ok := rerpc.AsError(err)
	assert.True(t, ok, "conversion to *rerpc.Error")
	return rerr
}

func assertErrorTwirp(t testing.TB, err error, msg string) twirp.Error {
	t.Helper()
	assert.NotNil(t, err, msg)
	twerr, ok := err.(twirp.Error)
	assert.True(t, ok, "conversion to twirp.Error")
	return twerr
}

func testWithReRPCClient(t *testing.T, client crosspb.CrossServiceClientReRPC) {
	t.Run("ping", func(t *testing.T) {
		num := rand.Int63()
		req := &crosspb.PingRequest{Number: num}
		expect := &crosspb.PingResponse{Number: num}
		res, err := client.Ping(context.Background(), req)
		assert.Nil(t, err, "ping error")
		assert.Equal(t, res, expect, "ping response")
	})
	t.Run("errors", func(t *testing.T) {
		req := &crosspb.FailRequest{Code: int32(rerpc.CodeResourceExhausted)}
		res, err := client.Fail(context.Background(), req)
		assert.Nil(t, res, "fail RPC response")
		rerr := assertErrorReRPC(t, err, "fail RPC error")
		assert.Equal(t, rerr.Code(), rerpc.CodeResourceExhausted, "error code")
		assert.Equal(t, rerr.Error(), "ResourceExhausted: "+errMsg, "error message")
		assert.Zero(t, rerr.Details(), "error details")
	})
	t.Run("cancel", func(t *testing.T) {
		req := &crosspb.PingRequest{}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		cancel()
		_, err := client.Ping(ctx, req)
		rerr := assertErrorReRPC(t, err, "error after canceling context")
		assert.Equal(t, rerr.Code(), rerpc.CodeCanceled, "error code")
		assert.Equal(t, rerr.Error(), "Canceled: context canceled", "error message")
	})
	t.Run("exceed_deadline", func(t *testing.T) {
		req := &crosspb.PingRequest{Sleep: durationpb.New(time.Second)}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := client.Ping(ctx, req)
		rerr := assertErrorReRPC(t, err, "deadline exceeded error")
		assert.Equal(t, rerr.Code(), rerpc.CodeDeadlineExceeded, "error code")
		assert.ErrorIs(t, rerr, context.DeadlineExceeded, "error unwraps to context.DeadlineExceeded")
	})
	t.Run("sum", func(t *testing.T) {
		const upTo = 10
		const expect = 55 // 1+10 + 2+9 + ... + 5+6 = 55
		stream := client.Sum(context.Background())
		for i := int64(1); i <= upTo; i++ {
			err := stream.Send(&crosspb.SumRequest{Number: i})
			assert.Nil(t, err, "Send %v", assert.Fmt(i))
		}
		res, err := stream.CloseAndReceive()
		assert.Nil(t, err, "CloseAndReceive error")
		assert.Equal(t, res, &crosspb.SumResponse{Sum: expect}, "response")
	})
	t.Run("count_up", func(t *testing.T) {
		const n = 5
		got := make([]int64, 0, n)
		expect := make([]int64, 0, n)
		for i := 1; i <= n; i++ {
			expect = append(expect, int64(i))
		}
		stream, err := client.CountUp(
			context.Background(),
			&crosspb.CountUpRequest{Number: n},
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
	t.Run("cumsum", func(t *testing.T) {
		send := []int64{3, 5, 1}
		expect := []int64{3, 8, 9}
		var got []int64
		stream := client.CumSum(context.Background())
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i, n := range send {
				err := stream.Send(&crosspb.CumSumRequest{Number: n})
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

func testWithGRPCClient(t *testing.T, client crosspb.CrossServiceClient, opts ...grpc.CallOption) {
	t.Run("ping", func(t *testing.T) {
		num := rand.Int63()
		req := &crosspb.PingRequest{Number: num}
		expect := &crosspb.PingResponse{Number: num}
		res, err := client.Ping(context.Background(), req, opts...)
		assert.Nil(t, err, "ping error")
		assert.Equal(t, res, expect, "ping response")
	})
	t.Run("errors", func(t *testing.T) {
		req := &crosspb.FailRequest{Code: int32(rerpc.CodeResourceExhausted)}
		_, err := client.Fail(context.Background(), req, opts...)
		s := assertErrorGRPC(t, err, "fail RPC error")
		assert.Equal(t, s.Code(), codes.ResourceExhausted, "error code")
		assert.Equal(t, s.Message(), errMsg, "error message")
		assert.Equal(t, s.Details(), []interface{}{}, "error details")
	})
	t.Run("cancel", func(t *testing.T) {
		req := &crosspb.PingRequest{}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		cancel()
		_, err := client.Ping(ctx, req, opts...)
		s := assertErrorGRPC(t, err, "error after canceling context")
		assert.Equal(t, s.Code(), codes.Canceled, "error code")
		// Generally bad practice to assert error messages we don't own, but
		// we'll want to keep rerpc's message in sync with grpc-go's.
		assert.Equal(t, s.Message(), "context canceled", "error message")
	})
	t.Run("exceed_deadline", func(t *testing.T) {
		req := &crosspb.PingRequest{Sleep: durationpb.New(time.Second)}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := client.Ping(ctx, req, opts...)
		s := assertErrorGRPC(t, err, "deadline exceeded error")
		assert.Equal(t, s.Code(), codes.DeadlineExceeded, "error code")
		// Generally bad practice to assert error messages we don't own, but
		// we'll want to keep rerpc's message in sync with grpc-go's.
		assert.Equal(t, s.Message(), "context deadline exceeded", "error message")
	})
	t.Run("sum", func(t *testing.T) {
		const upTo = 10
		const expect = 55 // 1+10 + 2+9 + ... + 5+6 = 55
		stream, err := client.Sum(context.Background())
		assert.Nil(t, err, "call error")
		for i := int64(1); i <= upTo; i++ {
			err := stream.Send(&crosspb.SumRequest{Number: i})
			assert.Nil(t, err, "Send %v", assert.Fmt(i))
		}
		res, err := stream.CloseAndRecv()
		assert.Nil(t, err, "CloseAndRecv error")
		assert.Equal(t, res, &crosspb.SumResponse{Sum: expect}, "response")
	})
	t.Run("count_up", func(t *testing.T) {
		const n = 5
		got := make([]int64, 0, n)
		expect := make([]int64, 0, n)
		for i := 1; i <= n; i++ {
			expect = append(expect, int64(i))
		}
		stream, err := client.CountUp(
			context.Background(),
			&crosspb.CountUpRequest{Number: n},
		)
		assert.Nil(t, err, "call error")
		for {
			msg, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			assert.Nil(t, err, "receive error")
			got = append(got, msg.Number)
		}
		assert.Equal(t, got, expect, "responses")
	})
	t.Run("cumsum", func(t *testing.T) {
		send := []int64{3, 5, 1}
		expect := []int64{3, 8, 9}
		var got []int64
		stream, err := client.CumSum(context.Background())
		assert.Nil(t, err, "call error")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i, n := range send {
				err := stream.Send(&crosspb.CumSumRequest{Number: n})
				assert.Nil(t, err, "send error #%v", assert.Fmt(i))
			}
			assert.Nil(t, stream.CloseSend(), "close send error")
		}()
		go func() {
			defer wg.Done()
			for {
				msg, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				assert.Nil(t, err, "receive error")
				got = append(got, msg.Sum)
			}
		}()
		wg.Wait()
		assert.Equal(t, got, expect, "sums")
	})
}

func testWithTwirpClient(t *testing.T, client crosspb.CrossService) {
	t.Run("ping", func(t *testing.T) {
		num := rand.Int63()
		req := &crosspb.PingRequest{Number: num}
		expect := &crosspb.PingResponse{Number: num}
		res, err := client.Ping(context.Background(), req)
		assert.Nil(t, err, "ping error")
		assert.Equal(t, res, expect, "ping response")
	})
	t.Run("errors", func(t *testing.T) {
		req := &crosspb.FailRequest{Code: int32(rerpc.CodeResourceExhausted)}
		_, err := client.Fail(context.Background(), req)
		twerr := assertErrorTwirp(t, err, "fail RPC error")
		assert.Equal(t, twerr.Code(), twirp.ResourceExhausted, "error code")
		assert.Equal(t, twerr.Msg(), errMsg, "error message")
		assert.Equal(t, len(twerr.MetaMap()), 0, "error metadata len")
	})
	t.Run("cancel", func(t *testing.T) {
		req := &crosspb.PingRequest{}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		cancel()
		_, err := client.Ping(ctx, req)
		twerr := assertErrorTwirp(t, err, "error after canceling context")
		// As of Twirp 8.1, clients return twirp.Internal if the context is
		// canceled before the request is sent. This seems strange.
		assert.Equal(t, twerr.Code(), twirp.Internal, "error code")
		assert.ErrorIs(t, twerr, context.Canceled, "underlying error")
	})
	t.Run("exceed_deadline", func(t *testing.T) {
		req := &crosspb.PingRequest{Sleep: durationpb.New(time.Second)}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := client.Ping(ctx, req)
		twerr := assertErrorTwirp(t, err, "deadline exceeded error")
		// As of Twirp 8.1, clients also return twirp.Internal if the deadline is
		// exceeded. Again, this seems strange.
		assert.Equal(t, twerr.Code(), twirp.Internal, "error code")
		assert.ErrorIs(t, twerr, context.DeadlineExceeded, "underlying error")
	})
}

func TestReRPCServer(t *testing.T) {
	reg := rerpc.NewRegistrar()
	mux := rerpc.NewServeMux(
		crosspb.NewCrossServiceHandlerReRPC(crossServerReRPC{}, reg),
		reflection.NewHandler(reg),
	)
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	t.Run("rerpc_client", func(t *testing.T) {
		t.Run("gzip", func(t *testing.T) {
			client := crosspb.NewCrossServiceClientReRPC(server.URL, server.Client(), rerpc.Gzip(true))
			testWithReRPCClient(t, client)
		})
		t.Run("identity", func(t *testing.T) {
			client := crosspb.NewCrossServiceClientReRPC(server.URL, server.Client())
			testWithReRPCClient(t, client)
		})
	})
	t.Run("grpc_client", func(t *testing.T) {
		pool := x509.NewCertPool()
		pool.AddCert(server.Certificate())
		gconn, err := grpc.Dial(
			server.Listener.Addr().String(),
			grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(pool, "" /* server name */)),
		)
		assert.Nil(t, err, "grpc dial")
		defer gconn.Close()
		client := crosspb.NewCrossServiceClient(gconn)
		t.Run("identity", func(t *testing.T) {
			testWithGRPCClient(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			testWithGRPCClient(t, client, grpc.UseCompressor(gzip.Name))
		})
		t.Run("reflection", func(t *testing.T) {
			client := grpcreflect.NewClient(context.Background(), grpb.NewServerReflectionClient(gconn))
			source := grpcurl.DescriptorSourceFromServer(context.Background(), client)
			out := &bytes.Buffer{}
			eventHandler := grpcurl.NewDefaultEventHandler(
				out,
				source,
				grpcurl.NewTextFormatter(false /* separator */),
				false, // verbose
			)
			var calls int
			msg := func(msg protov1.Message) error {
				calls++
				if calls == 1 {
					return jsonpbv1.UnmarshalString(`{"number": "42"}`, msg)
				}
				return io.EOF
			}
			err := grpcurl.InvokeRPC(
				context.Background(),
				source,
				gconn,
				"internal.crosstest.v1test.CrossService.Ping",
				nil, // headers
				eventHandler,
				msg, // request data
			)
			assert.Nil(t, err, "grpcurl error")
			t.Log(out.String())
			assert.True(t, strings.Contains(out.String(), "number: 42"), "grpcurl output")
		})
	})
	t.Run("twirp_client", func(t *testing.T) {
		opts := []twirp.ClientOption{
			twirp.WithClientPathPrefix(""),    // by default, reRPC doesn't use a prefix
			twirp.WithClientLiteralURLs(true), // Twirp clients don't follow spec by default
		}
		t.Run("json", func(t *testing.T) {
			client := crosspb.NewCrossServiceJSONClient(server.URL, server.Client(), opts...)
			testWithTwirpClient(t, client)
		})
		t.Run("protobuf", func(t *testing.T) {
			client := crosspb.NewCrossServiceProtobufClient(server.URL, server.Client(), opts...)
			testWithTwirpClient(t, client)
		})
	})
}

func TestReRPCServerH2C(t *testing.T) {
	mux := rerpc.NewServeMux(crosspb.NewCrossServiceHandlerReRPC(crossServerReRPC{}))
	server := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	defer server.Close()

	t.Run("rerpc_client", func(t *testing.T) {
		hclient := newClientH2C()
		t.Run("identity", func(t *testing.T) {
			client := crosspb.NewCrossServiceClientReRPC(server.URL, hclient)
			testWithReRPCClient(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			client := crosspb.NewCrossServiceClientReRPC(server.URL, hclient, rerpc.Gzip(true))
			testWithReRPCClient(t, client)
		})
	})
	t.Run("grpc_client", func(t *testing.T) {
		gconn, err := grpc.Dial(
			server.Listener.Addr().String(),
			grpc.WithInsecure(),
		)
		assert.Nil(t, err, "grpc dial")
		client := crosspb.NewCrossServiceClient(gconn)
		t.Run("identity", func(t *testing.T) {
			testWithGRPCClient(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			testWithGRPCClient(t, client, grpc.UseCompressor(gzip.Name))
		})
	})
	t.Run("twirp_client", func(t *testing.T) {
		opts := []twirp.ClientOption{
			twirp.WithClientPathPrefix(""),    // by default, reRPC doesn't use a prefix
			twirp.WithClientLiteralURLs(true), // Twirp clients don't follow spec by default
		}
		t.Run("json", func(t *testing.T) {
			client := crosspb.NewCrossServiceJSONClient(server.URL, server.Client(), opts...)
			testWithTwirpClient(t, client)
		})
		t.Run("protobuf", func(t *testing.T) {
			client := crosspb.NewCrossServiceProtobufClient(server.URL, server.Client(), opts...)
			testWithTwirpClient(t, client)
		})
	})
}

func TestGRPCServer(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	assert.Nil(t, err, "listen on ephemeral port")
	server := grpc.NewServer()
	crosspb.RegisterCrossServiceServer(server, crossServerGRPC{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(lis)
	}()
	defer wg.Wait()
	defer server.GracefulStop()

	t.Run("rerpc_client", func(t *testing.T) {
		hclient := newClientH2C()
		t.Run("identity", func(t *testing.T) {
			client := crosspb.NewCrossServiceClientReRPC("http://"+lis.Addr().String(), hclient)
			testWithReRPCClient(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			client := crosspb.NewCrossServiceClientReRPC("http://"+lis.Addr().String(), hclient, rerpc.Gzip(true))
			testWithReRPCClient(t, client)
		})
	})
	t.Run("grpc_client", func(t *testing.T) {
		gconn, err := grpc.Dial(
			lis.Addr().String(),
			grpc.WithInsecure(),
		)
		assert.Nil(t, err, "grpc dial")
		client := crosspb.NewCrossServiceClient(gconn)
		t.Run("identity", func(t *testing.T) {
			testWithGRPCClient(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			testWithGRPCClient(t, client, grpc.UseCompressor(gzip.Name))
		})
	})
}
