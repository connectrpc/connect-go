package rerpc_test

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/akshayjshah/rerpc"
	pingpb "github.com/akshayjshah/rerpc/internal/ping/v1test"
)

func ExampleClient() {
	// Timeouts, connection pooling, custom dialers, and other low-level
	// transport details are handled by net/http. Everything you already know
	// (or everything you learn) about hardening net/http Clients applies to
	// reRPC too.
	//
	// Of course, you can skip this configuration and use http.DefaultClient for
	// quick proof-of-concept code.
	hc := &http.Client{
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
	short := rerpc.ShortCircuit(rerpc.Errorf(rerpc.CodeUnimplemented, "no networking in examples"))

	client := pingpb.NewPingServiceClientReRPC("http://invalid-test-url", hc, rerpc.NewChain(short))
	res, err := client.Ping(context.Background(), &pingpb.PingRequest{})
	fmt.Println("Response:", res)
	fmt.Println("Error:", err)

	// Output:
	// Response: <nil>
	// Error: Unimplemented: no networking in examples
}
