package connect_test

import (
	"context"
	"net/http"
	"time"

	"github.com/bufbuild/connect"
	"github.com/bufbuild/connect/health"
	pingrpc "github.com/bufbuild/connect/internal/gen/proto/go-connect/connect/ping/v1test"
	pingpb "github.com/bufbuild/connect/internal/gen/proto/go/connect/ping/v1test"
	"github.com/bufbuild/connect/reflection"
)

// ExamplePingServer implements some trivial business logic. The protobuf
// definition for this API is in proto/connect/ping/v1test/ping.proto.
type ExamplePingServer struct {
	pingrpc.UnimplementedPingServiceHandler
}

// Ping implements pingrpc.PingServiceHandler.
func (*ExamplePingServer) Ping(
	_ context.Context,
	req *connect.Request[pingpb.PingRequest],
) (*connect.Response[pingpb.PingResponse], error) {
	return connect.NewResponse(&pingpb.PingResponse{
		Number: req.Msg.Number,
		Text:   req.Msg.Text,
	}), nil
}

func Example() {
	// The business logic here is trivial, but the rest of the example is meant
	// to be somewhat realistic. This server has basic timeouts configured, and
	// it also exposes gRPC's server reflection and health check APIs.
	reg := connect.NewRegistrar()     // for gRPC reflection
	checker := health.NewChecker(reg) // basic health checks

	// The generated code produces plain net/http Handlers, so they're compatible
	// with most Go HTTP routers and middleware (for example, net/http's
	// StripPrefix).
	mux := http.NewServeMux()
	mux.Handle(pingrpc.NewPingServiceHandler(
		&ExamplePingServer{},                // our business logic
		connect.WithRegistrar(reg),          // register the ping service's types
		connect.WithReadMaxBytes(1024*1024), // limit request size
	))
	mux.Handle(reflection.NewHandler(reg)) // server reflection
	mux.Handle(health.NewHandler(checker)) // health checks

	// Timeouts, connection handling, TLS configuration, and other low-level
	// transport details are handled by net/http. Everything you already know (or
	// anything you learn) about hardening net/http Servers applies to connect
	// too. Keep in mind that any timeouts you set will also apply to streaming
	// RPCs!
	//
	// If you're not familiar with the many timeouts exposed by net/http, start with
	// https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/.
	srv := &http.Server{
		Addr:           ":http",
		Handler:        mux,
		ReadTimeout:    2500 * time.Millisecond,
		WriteTimeout:   5 * time.Second,
		MaxHeaderBytes: 8 * 1024, // 8KiB, gRPC's recommendation
	}
	// You could also use golang.org/x/net/http2/h2c to serve gRPC requests
	// without TLS.
	srv.ListenAndServeTLS("testdata/server.crt", "testdata/server.key")
}
