package connect_test

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/bufconnect/connect"
	pingrpc "github.com/bufconnect/connect/internal/gen/proto/go-connect/connect/ping/v1test"
	pingpb "github.com/bufconnect/connect/internal/gen/proto/go/connect/ping/v1test"
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

	// This interceptor stops the client from making HTTP requests in examples.
	// Leave it out in real code!
	short := ShortCircuit(connect.Errorf(connect.CodeUnimplemented, "no networking in examples"))

	client, err := pingrpc.NewPingServiceClient(
		"http://invalid-test-url",
		httpClient,
		connect.Interceptors(short),
	)
	if err != nil {
		logger.Print("Error: ", err)
		return
	}
	res, err := client.Ping(
		context.Background(),
		connect.NewRequest(&pingpb.PingRequest{}),
	)
	if err != nil {
		logger.Print("Error: ", err)
		return
	}
	logger.Print("Response headers:", res.Header())
	logger.Print("Response message:", res.Msg)

	// Output:
	// Error: Unimplemented: no networking in examples
}
