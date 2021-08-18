package crosstest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding/gzip"

	"github.com/rerpc/rerpc"
	"github.com/rerpc/rerpc/internal/assert"
	crosspb "github.com/rerpc/rerpc/internal/crosstest/v1test"
)

func BenchmarkReRPC(b *testing.B) {
	mux := http.NewServeMux()
	mux.Handle(crosspb.NewCrossServiceHandlerReRPC(crossServerReRPC{}))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	client := crosspb.NewCrossServiceClientReRPC(server.URL, server.Client(), rerpc.Gzip(true))
	b.ResetTimer()

	b.Run("unary", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = client.Ping(context.Background(), &crosspb.PingRequest{Number: 42})
			}
		})
	})
}

func BenchmarkGRPC(b *testing.B) {
	lis, err := net.Listen("tcp", "localhost:0")
	assert.Nil(b, err, "listen on ephemeral port")
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

	gconn, err := grpc.Dial(
		lis.Addr().String(),
		grpc.WithInsecure(),
	)
	assert.Nil(b, err, "grpc dial")
	client := crosspb.NewCrossServiceClient(gconn)

	b.ResetTimer()

	b.Run("unary", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = client.Ping(context.Background(), &crosspb.PingRequest{Number: 42}, grpc.UseCompressor(gzip.Name))
			}
		})
	})
}
