package crosstest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
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
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials" // register gzip compressor
	grpcgzip "google.golang.org/grpc/encoding/gzip"
	grpb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/bufbuild/connect"
	connectgzip "github.com/bufbuild/connect/compress/gzip"
	"github.com/bufbuild/connect/handlerstream"
	"github.com/bufbuild/connect/internal/assert"
	crossrpc "github.com/bufbuild/connect/internal/crosstest/gen/proto/go-connect/cross/v1test"
	crosspb "github.com/bufbuild/connect/internal/crosstest/gen/proto/go/cross/v1test"
	"github.com/bufbuild/connect/reflection"
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

type crossServerConnect struct {
	crossrpc.UnimplementedCrossServiceHandler
}

func (c crossServerConnect) Ping(ctx context.Context, req *connect.Envelope[crosspb.PingRequest]) (*connect.Envelope[crosspb.PingResponse], error) {
	if err := req.Msg.Sleep.CheckValid(); req.Msg.Sleep != nil && err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if d := req.Msg.Sleep.AsDuration(); d > 0 {
		time.Sleep(d)
	}
	return connect.NewEnvelope(&crosspb.PingResponse{Number: req.Msg.Number}), nil
}

func (c crossServerConnect) Fail(ctx context.Context, req *connect.Envelope[crosspb.FailRequest]) (*connect.Envelope[crosspb.FailResponse], error) {
	return nil, connect.NewError(connect.CodeResourceExhausted, errors.New(errMsg))
}

func (c crossServerConnect) Sum(
	ctx context.Context,
	stream *handlerstream.Client[crosspb.SumRequest, crosspb.SumResponse],
) error {
	var sum int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(connect.NewEnvelope(&crosspb.SumResponse{
				Sum: sum,
			}))
		} else if err != nil {
			return err
		}
		sum += msg.Number
	}
}

func (c crossServerConnect) CountUp(
	ctx context.Context,
	req *connect.Envelope[crosspb.CountUpRequest],
	stream *handlerstream.Server[crosspb.CountUpResponse],
) error {
	if req.Msg.Number <= 0 {
		return connect.NewError(
			connect.CodeInvalidArgument,
			fmt.Errorf("number must be positive: got %v", req.Msg.Number),
		)
	}
	for i := int64(1); i <= req.Msg.Number; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := stream.Send(&crosspb.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
	return nil
}

func (c crossServerConnect) CumSum(
	ctx context.Context,
	stream *handlerstream.Bidirectional[crosspb.CumSumRequest, crosspb.CumSumResponse],
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

func assertErrorConnect(t testing.TB, err error, msg string) *connect.Error {
	t.Helper()
	assert.NotNil(t, err, msg)
	var connectErr *connect.Error
	ok := errors.As(err, &connectErr)
	assert.True(t, ok, "conversion to *connect.Error")
	return connectErr
}

func testWithConnectClient(t *testing.T, client crossrpc.CrossServiceClient) {
	t.Run("ping", func(t *testing.T) {
		num := rand.Int63()
		req := &crosspb.PingRequest{Number: num}
		expect := &crosspb.PingResponse{Number: num}
		res, err := client.Ping(context.Background(), connect.NewEnvelope(req))
		assert.Nil(t, err, "ping error")
		assert.Equal(t, res.Msg, expect, "ping response")
	})
	t.Run("errors", func(t *testing.T) {
		req := &crosspb.FailRequest{Code: int32(connect.CodeResourceExhausted)}
		res, err := client.Fail(context.Background(), connect.NewEnvelope(req))
		assert.Nil(t, res, "fail RPC response")
		cerr := assertErrorConnect(t, err, "fail RPC error")
		assert.Equal(t, cerr.Code(), connect.CodeResourceExhausted, "error code")
		assert.Equal(t, cerr.Error(), "ResourceExhausted: "+errMsg, "error message")
		assert.Zero(t, cerr.Details(), "error details")
	})
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		cancel()
		_, err := client.Ping(ctx, connect.NewEnvelope(&crosspb.PingRequest{}))
		cerr := assertErrorConnect(t, err, "error after canceling context")
		assert.Equal(t, cerr.Code(), connect.CodeCanceled, "error code")
		assert.Equal(t, cerr.Error(), "Canceled: context canceled", "error message")
	})
	t.Run("exceed_deadline", func(t *testing.T) {
		req := &crosspb.PingRequest{Sleep: durationpb.New(time.Second)}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := client.Ping(ctx, connect.NewEnvelope(req))
		cerr := assertErrorConnect(t, err, "deadline exceeded error")
		assert.Equal(t, cerr.Code(), connect.CodeDeadlineExceeded, "error code")
		assert.ErrorIs(t, cerr, context.DeadlineExceeded, "error unwraps to context.DeadlineExceeded")
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
		assert.Equal(t, res.Msg, &crosspb.SumResponse{Sum: expect}, "response")
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
			connect.NewEnvelope(&crosspb.CountUpRequest{Number: n}),
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
		req := &crosspb.FailRequest{Code: int32(connect.CodeResourceExhausted)}
		_, err := client.Fail(context.Background(), req, opts...)
		s := assertErrorGRPC(t, err, "fail RPC error")
		assert.Equal(t, s.Code(), codes.ResourceExhausted, "error code")
		assert.Equal(t, s.Message(), errMsg, "error message")
		assert.Equal(t, s.Details(), []any{}, "error details")
	})
	t.Run("cancel", func(t *testing.T) {
		req := &crosspb.PingRequest{}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		cancel()
		_, err := client.Ping(ctx, req, opts...)
		s := assertErrorGRPC(t, err, "error after canceling context")
		assert.Equal(t, s.Code(), codes.Canceled, "error code")
		// Generally bad practice to assert error messages we don't own, but
		// we'll want to keep connect's message in sync with grpc-go's.
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
		// we'll want to keep connect's message in sync with grpc-go's.
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

func TestConnectServer(t *testing.T) {
	reg := connect.NewRegistrar()
	mux := http.NewServeMux()
	mux.Handle(crossrpc.NewCrossServiceHandler(
		crossServerConnect{},
		connect.WithRegistrar(reg),
	))
	mux.Handle(reflection.NewHandler(reg))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	t.Run("connect_client", func(t *testing.T) {
		t.Run("gzip", func(t *testing.T) {
			client, err := crossrpc.NewCrossServiceClient(
				server.URL,
				server.Client(),
				connect.WithRequestCompressor(connectgzip.Name),
			)
			assert.Nil(t, err, "client construction error")
			testWithConnectClient(t, client)
		})
		t.Run("identity", func(t *testing.T) {
			client, err := crossrpc.NewCrossServiceClient(server.URL, server.Client())
			assert.Nil(t, err, "client construction error")
			testWithConnectClient(t, client)
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
			testWithGRPCClient(t, client, grpc.UseCompressor(grpcgzip.Name))
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
				"cross.v1test.CrossService.Ping",
				nil, // headers
				eventHandler,
				msg, // request data
			)
			assert.Nil(t, err, "grpcurl error")
			t.Log(out.String())
			assert.True(t, strings.Contains(out.String(), "number: 42"), "grpcurl output")
		})
	})
}

func TestConnectServerH2C(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(crossrpc.NewCrossServiceHandler(crossServerConnect{}))
	server := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	defer server.Close()

	t.Run("connect_client", func(t *testing.T) {
		hclient := newClientH2C()
		t.Run("identity", func(t *testing.T) {
			client, err := crossrpc.NewCrossServiceClient(server.URL, hclient)
			assert.Nil(t, err, "client construction error")
			testWithConnectClient(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			client, err := crossrpc.NewCrossServiceClient(
				server.URL,
				hclient,
				connect.WithRequestCompressor(connectgzip.Name),
			)
			assert.Nil(t, err, "client construction error")
			testWithConnectClient(t, client)
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
			testWithGRPCClient(t, client, grpc.UseCompressor(grpcgzip.Name))
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

	t.Run("connect_client", func(t *testing.T) {
		hclient := newClientH2C()
		url := "http://" + lis.Addr().String()
		t.Run("identity", func(t *testing.T) {
			client, err := crossrpc.NewCrossServiceClient(url, hclient)
			assert.Nil(t, err, "client construction error")
			testWithConnectClient(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			client, err := crossrpc.NewCrossServiceClient(
				url,
				hclient,
				connect.WithRequestCompressor(connectgzip.Name),
			)
			assert.Nil(t, err, "client construction error")
			testWithConnectClient(t, client)
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
			testWithGRPCClient(t, client, grpc.UseCompressor(grpcgzip.Name))
		})
	})
}
