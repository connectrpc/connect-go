package rerpc_test

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/rerpc/rerpc"
	pingrpc "github.com/rerpc/rerpc/internal/gen/proto/go-rerpc/rerpc/ping/v1test"
	pingpb "github.com/rerpc/rerpc/internal/gen/proto/go/rerpc/ping/v1test"
)

func ExampleClient() {
	logger := log.New(os.Stdout, "" /* prefix */, 0 /* flags */)
	// Timeouts, connection pooling, custom dialers, and other low-level
	// transport details are handled by net/http. Everything you already know
	// (or everything you learn) about hardening net/http Clients applies to
	// reRPC too.
	//
	// Of course, you can skip this configuration and use http.DefaultClient for
	// quick proof-of-concept code.
	doer := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			// reRPC handles compression negotiation.
			DisableCompression: true,
			MaxIdleConns:       128,
			// RPC clients tend to make many requests to few hosts, so allow more
			// idle connections per host.
			MaxIdleConnsPerHost:    16,
			IdleConnTimeout:        90 * time.Second,
			MaxResponseHeaderBytes: rerpc.MaxHeaderBytes,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// Don't follow any redirects.
			return http.ErrUseLastResponse
		},
	}

	// This interceptor stops the client from making HTTP requests in examples.
	// Leave it out in real code!
	short := ShortCircuit(rerpc.Errorf(rerpc.CodeUnimplemented, "no networking in examples"))

	client, err := pingrpc.NewPingServiceClient("http://invalid-test-url", doer, rerpc.Intercept(short))
	if err != nil {
		logger.Print("Error: ", err)
		return
	}
	res, err := client.Ping(context.Background(), &pingpb.PingRequest{})
	if err != nil {
		logger.Print("Error: ", err)
		return
	}
	logger.Print("Response:", res)

	// Output:
	// Error: Unimplemented: no networking in examples
}
