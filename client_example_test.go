package connect_test

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/bufbuild/connect"
	pingrpc "github.com/bufbuild/connect/internal/gen/proto/connect/connect/ping/v1test"
	pingpb "github.com/bufbuild/connect/internal/gen/proto/go/connect/ping/v1test"
)

func ExampleClient() {
	logger := log.New(os.Stdout, "" /* prefix */, 0 /* flags */)
	// Timeouts, connection pooling, custom dialers, and other low-level
	// transport details are handled by net/http. Everything you already know
	// (or everything you learn) about hardening net/http Clients applies to
	// connect too.
	//
	// Of course, you can skip this configuration and use http.DefaultClient for
	// quick proof-of-concept code.
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			// connect handles compression on a per-message basis, so it's a waste to
			// compress the whole response body.
			DisableCompression: true,
			MaxIdleConns:       128,
			// RPC clients tend to make many requests to few hosts, so allow more
			// idle connections per host.
			MaxIdleConnsPerHost:    16,
			IdleConnTimeout:        90 * time.Second,
			MaxResponseHeaderBytes: 8 * 1024, // 8 KiB, gRPC's recommended setting
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// Don't follow any redirects.
			return http.ErrUseLastResponse
		},
	}
	// Unfortunately, pkg.go.dev can't run examples that actually use the
	// network. To keep this example runnable, we'll use an HTTP server and
	// client that communicate over in-memory pipes. Don't do this in production!
	httpClient = examplePingServer.Client()

	client, err := pingrpc.NewPingServiceClient(
		examplePingServer.URL(),
		httpClient,
	)
	if err != nil {
		logger.Println("error:", err)
		return
	}
	res, err := client.Ping(
		context.Background(),
		connect.NewEnvelope(&pingpb.PingRequest{Number: 42}),
	)
	if err != nil {
		logger.Println("error:", err)
		return
	}
	logger.Println("response content-type:", res.Header().Get("Content-Type"))
	logger.Println("response message:", res.Msg)

	// Output:
	// response content-type: application/grpc+proto
	// response message: number:42
}
