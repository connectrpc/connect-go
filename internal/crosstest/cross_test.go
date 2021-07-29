package crosstest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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

	"github.com/akshayjshah/rerpc"
	"github.com/akshayjshah/rerpc/internal/assert"
	crosspb "github.com/akshayjshah/rerpc/internal/crosstest/v1test"
)

const errMsg = "soirée 🎉" // readable non-ASCII

type combinedError struct {
	err *rerpc.Error
}

func NewCombinedError(err *rerpc.Error) error {
	return &combinedError{err}
}

func (c *combinedError) Unwrap() error {
	return c.err
}

func (c *combinedError) Error() string {
	return c.err.Error()
}

func (c *combinedError) GRPCStatus() *status.Status {
	if c.err == nil {
		return nil
	}
	msg := strings.SplitN(c.err.Error(), ": ", 2)[1]
	return status.New(codes.Code(c.err.Code()), msg)
}

type crossServer struct {
	crosspb.UnimplementedCrossServiceServer
	crosspb.UnimplementedCrossServiceReRPC
}

func (c crossServer) Ping(ctx context.Context, req *crosspb.PingRequest) (*crosspb.PingResponse, error) {
	if err := req.Sleep.CheckValid(); req.Sleep != nil && err != nil {
		return nil, NewCombinedError(rerpc.Errorf(rerpc.CodeInvalidArgument, err.Error()).(*rerpc.Error))
	}
	if d := req.Sleep.AsDuration(); d > 0 {
		time.Sleep(d)
	}
	return &crosspb.PingResponse{Number: req.Number}, nil
}

func (c crossServer) Fail(ctx context.Context, req *crosspb.FailRequest) (*crosspb.FailResponse, error) {
	return nil, NewCombinedError(rerpc.Errorf(rerpc.CodeResourceExhausted, errMsg).(*rerpc.Error))
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
		assert.Equal(t, rerr.Error(), "DeadlineExceeded: context deadline exceeded", "error message")
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
	mux := http.NewServeMux()
	mux.Handle(crosspb.NewCrossServiceHandlerReRPC(crossServer{}, reg))
	mux.Handle(rerpc.NewReflectionHandler(reg))
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
	mux := http.NewServeMux()
	mux.Handle(crosspb.NewCrossServiceHandlerReRPC(crossServer{}))
	server := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	defer server.Close()

	t.Run("rerpc_client", func(t *testing.T) {
		t.Run("identity", func(t *testing.T) {
			client := crosspb.NewCrossServiceClientReRPC(server.URL, server.Client())
			testWithReRPCClient(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			client := crosspb.NewCrossServiceClientReRPC(server.URL, server.Client(), rerpc.Gzip(true))
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
	crosspb.RegisterCrossServiceServer(server, crossServer{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(lis)
	}()
	defer wg.Wait()
	defer server.GracefulStop()

	t.Run("rerpc_client", func(t *testing.T) {
		// This is wildly insecure - don't do this in production!
		hclient := &http.Client{
			Transport: &http2.Transport{
				AllowHTTP: true,
				DialTLS: func(netw, addr string, cfg *tls.Config) (net.Conn, error) {
					return net.Dial(netw, addr)
				},
			},
		}
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
