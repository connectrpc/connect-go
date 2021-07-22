package crosstest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials" // register gzip compressor
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/status"

	"github.com/akshayjshah/rerpc"
	"github.com/akshayjshah/rerpc/internal/assert"
	"github.com/akshayjshah/rerpc/internal/crosstest/crosspb/v0"
)

const errMsg = "oh no"

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
	crosspb.UnimplementedCrosstestServer
	crosspb.UnimplementedCrosstestServerReRPC
}

func (c crossServer) Ping(ctx context.Context, req *crosspb.PingRequest) (*crosspb.PingResponse, error) {
	return &crosspb.PingResponse{Number: req.Number}, nil
}

func (c crossServer) Fail(ctx context.Context, req *crosspb.FailRequest) (*crosspb.FailResponse, error) {
	return nil, NewCombinedError(rerpc.Errorf(rerpc.CodeResourceExhausted, errMsg).(*rerpc.Error))
}

func testWithReRPCClient(t *testing.T, client crosspb.CrosstestClientReRPC) {
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
		assert.NotNil(t, err, "fail RPC error")
		rerr, ok := rerpc.AsError(err)
		assert.True(t, ok, "conversion to *rerpc.Error")
		t.Log(rerr.Error())
		assert.Equal(t, rerr.Code(), rerpc.CodeResourceExhausted, "error code")
		assert.Equal(t, rerr.Error(), "ResourceExhausted: "+errMsg, "error message")
		assert.Zero(t, rerr.Details(), "error details")
	})
}

func testWithGRPCClient(t *testing.T, client crosspb.CrosstestClient, opts ...grpc.CallOption) {
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
		res, err := client.Fail(context.Background(), req, opts...)
		assert.Nil(t, res, "fail RPC response")
		assert.NotNil(t, err, "fail RPC error")
		s, ok := status.FromError(err)
		assert.True(t, ok, "conversion to *status.Status")
		assert.Equal(t, s.Code(), codes.ResourceExhausted, "error code")
		assert.Equal(t, s.Message(), errMsg, "error message")
		assert.Equal(t, s.Details(), []interface{}{}, "error details")
	})
}

func TestReRPCServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(crosspb.NewCrosstestHandlerReRPC(crossServer{}))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	t.Run("rerpc_client", func(t *testing.T) {
		t.Run("identity", func(t *testing.T) {
			client := crosspb.NewCrosstestClientReRPC(server.URL, server.Client())
			testWithReRPCClient(t, client)
		})
		// TODO: gzip
	})
	t.Run("grpc_client", func(t *testing.T) {
		pool := x509.NewCertPool()
		pool.AddCert(server.Certificate())
		gconn, err := grpc.Dial(
			server.Listener.Addr().String(),
			grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(pool, "" /* server name */)),
		)
		assert.Nil(t, err, "grpc dial")
		client := crosspb.NewCrosstestClient(gconn)
		t.Run("identity", func(t *testing.T) {
			testWithGRPCClient(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			testWithGRPCClient(t, client, grpc.UseCompressor(gzip.Name))
		})
	})
}

func TestReRPCServerH2C(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(crosspb.NewCrosstestHandlerReRPC(crossServer{}))
	server := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	defer server.Close()

	t.Run("rerpc_client", func(t *testing.T) {
		t.Run("identity", func(t *testing.T) {
			client := crosspb.NewCrosstestClientReRPC(server.URL, server.Client())
			testWithReRPCClient(t, client)
		})
		// TODO: gzip
	})
	t.Run("grpc_client", func(t *testing.T) {
		gconn, err := grpc.Dial(
			server.Listener.Addr().String(),
			grpc.WithInsecure(),
		)
		assert.Nil(t, err, "grpc dial")
		client := crosspb.NewCrosstestClient(gconn)
		t.Run("identity", func(t *testing.T) {
			testWithGRPCClient(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			testWithGRPCClient(t, client, grpc.UseCompressor(gzip.Name))
		})
	})
}

func TestGRPCServer(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	assert.Nil(t, err, "listen on ephemeral port")
	server := grpc.NewServer()
	crosspb.RegisterCrosstestServer(server, crossServer{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(lis)
	}()
	defer wg.Wait()
	defer server.GracefulStop()

	t.Run("rerpc_client", func(t *testing.T) {
		t.Run("identity", func(t *testing.T) {
			// This is wildly insecure!
			hclient := &http.Client{
				Transport: &http2.Transport{
					AllowHTTP: true,
					DialTLS: func(netw, addr string, cfg *tls.Config) (net.Conn, error) {
						return net.Dial(netw, addr)
					},
				},
			}
			client := crosspb.NewCrosstestClientReRPC("http://"+lis.Addr().String(), hclient)
			testWithReRPCClient(t, client)
		})
		// TODO: gzip
	})
	t.Run("grpc_client", func(t *testing.T) {
		gconn, err := grpc.Dial(
			lis.Addr().String(),
			grpc.WithInsecure(),
		)
		assert.Nil(t, err, "grpc dial")
		client := crosspb.NewCrosstestClient(gconn)
		t.Run("identity", func(t *testing.T) {
			testWithGRPCClient(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			testWithGRPCClient(t, client, grpc.UseCompressor(gzip.Name))
		})
	})
}
