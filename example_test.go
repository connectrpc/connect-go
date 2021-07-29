package rerpc_test

import (
	"context"
	"net/http"
	"time"

	"github.com/akshayjshah/rerpc"
	pingpb "github.com/akshayjshah/rerpc/internal/ping/v1test"
)

// ExamplePingServer implements some trivial business logic. The protobuf
// definition for this API is in internal/pingpb/ping.proto.
type ExamplePingServer struct {
	pingpb.UnimplementedPingServiceReRPC
}

// Ping implements pingpb.PingServerReRPC.
func (*ExamplePingServer) Ping(ctx context.Context, req *pingpb.PingRequest) (*pingpb.PingResponse, error) {
	return &pingpb.PingResponse{Number: req.Number}, nil
}

func Example() {
	// The business logic here is trivial, but the rest of the example is meant
	// to be somewhat realistic. This server has basic timeouts configured, and
	// it also supports gRPC's server reflection and health check APIs.
	ping := &ExamplePingServer{}     // our business logic
	reg := rerpc.NewRegistrar()      // for gRPC reflection
	checker := rerpc.NewChecker(reg) // basic health checks

	mux := http.NewServeMux()
	mux.Handle(pingpb.NewPingServiceHandlerReRPC(ping, reg)) // business logic
	mux.Handle(rerpc.NewReflectionHandler(reg))              // server reflection
	mux.Handle(rerpc.NewHealthHandler(checker, reg))         // health checks
	mux.Handle("/", rerpc.NewBadRouteHandler())              // Twirp-compatible 404s

	srv := &http.Server{
		Addr:           ":http",
		ReadTimeout:    2500 * time.Millisecond,
		WriteTimeout:   5 * time.Second,
		MaxHeaderBytes: rerpc.MaxHeaderBytes,
	}
	srv.ListenAndServe() // ListenAndServeTLS in production
}
