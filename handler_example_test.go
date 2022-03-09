// Copyright 2021-2022 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connect_test

import (
	"context"
	"net/http"
	"time"

	"github.com/bufbuild/connect"
	pingrpc "github.com/bufbuild/connect/internal/gen/connect/ping/v1"
	pingpb "github.com/bufbuild/connect/internal/gen/go/ping/v1"
)

// ExamplePingServer implements some trivial business logic. The protobuf
// definition for this API is in proto/ping/v1/ping.proto.
type ExamplePingServer struct {
	pingrpc.UnimplementedPingServiceHandler
}

// Ping implements pingrpc.PingServiceHandler.
func (*ExamplePingServer) Ping(
	_ context.Context,
	req *connect.Envelope[pingpb.PingRequest],
) (*connect.Envelope[pingpb.PingResponse], error) {
	return connect.NewEnvelope(&pingpb.PingResponse{
		Number: req.Msg.Number,
		Text:   req.Msg.Text,
	}), nil
}

func Example_handler() {
	// The business logic here is trivial, but the rest of the example is meant
	// to be somewhat realistic. This server has basic timeouts configured, and
	// it also exposes gRPC's server reflection and health check APIs.
	reg := connect.NewRegistrar()            // for gRPC reflection
	checker := connect.NewHealthChecker(reg) // basic health checks

	// The generated code produces plain net/http Handlers, so they're compatible
	// with most Go HTTP routers and middleware (for example, net/http's
	// StripPrefix).
	mux := http.NewServeMux()
	mux.Handle(pingrpc.NewPingServiceHandler(
		&ExamplePingServer{},                // our business logic
		connect.WithRegistrar(reg),          // register the ping service's types
		connect.WithReadMaxBytes(1024*1024), // limit request size
	))
	mux.Handle(connect.NewReflectionHandler(reg)) // server reflection
	mux.Handle(connect.NewHealthHandler(checker)) // health checks

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
