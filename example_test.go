// Copyright 2021-2023 Buf Technologies, Inc.
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
	"log"
	"net/http"
	"os"

	"github.com/bufbuild/connect-go"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
	"github.com/bufbuild/connect-go/internal/gen/connect/ping/v1/pingv1connect"
)

// examplePingServer implements some trivial business logic. The Protobuf
// definition for this API is in proto/connect/ping/v1/ping.proto.
type examplePingImplementation struct {
	pingv1connect.UnimplementedPingServiceHandler
}

// Ping implements pingv1connect.PingServiceHandler.
func (*examplePingImplementation) Ping(
	_ context.Context,
	request *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {
	return connect.NewResponse(
		&pingv1.PingResponse{
			Number: request.Msg.Number,
			Text:   request.Msg.Text,
		},
	), nil
}

func Example_handler() {
	// protoc-gen-connect-go generates constructors that return plain net/http
	// Handlers, so they're compatible with most Go HTTP routers and middleware
	// (for example, net/http's StripPrefix). Each handler automatically supports
	// the Connect, gRPC, and gRPC-Web protocols.
	mux := http.NewServeMux()
	mux.Handle(
		pingv1connect.NewPingServiceHandler(
			// Our business logic, implemented in example_test.go.
			&examplePingImplementation{},
		),
	)
	// You can serve gRPC's health and server reflection APIs using
	// github.com/bufbuild/connect-grpchealth-go and
	// github.com/bufbuild/connect-grpcreflect-go.
	_ = http.ListenAndServeTLS(
		"localhost:8080",
		"internal/testdata/server.crt",
		"internal/testdata/server.key",
		mux,
	)
	// To serve HTTP/2 requests without TLS (as many gRPC clients expect), import
	// golang.org/x/net/http2/h2c and golang.org/x/net/http2 and change to:
	// _ = http.ListenAndServe(
	// 	"localhost:8080",
	// 	h2c.NewHandler(mux, &http2.Server{}),
	// )
}

func Example_client() {
	logger := log.New(os.Stdout, "" /* prefix */, 0 /* flags */)
	// Unfortunately, pkg.go.dev can't run examples that actually use the
	// network. To keep this example runnable, we'll use an HTTP server and
	// client that communicate over in-memory pipes. The client is still a plain
	// *http.Client!
	var httpClient *http.Client = examplePingServer.Client()

	// By default, clients use the Connect protocol. Add connect.WithGRPC() or
	// connect.WithGRPCWeb() to switch protocols.
	client := pingv1connect.NewPingServiceClient(
		httpClient,
		examplePingServer.URL(),
	)
	response, err := client.Ping(
		context.Background(),
		connect.NewRequest(&pingv1.PingRequest{Number: 42}),
	)
	if err != nil {
		logger.Println("error:", err)
		return
	}
	logger.Println("response content-type:", response.Header().Get("Content-Type"))
	logger.Println("response message:", response.Msg)

	// Output:
	// response content-type: application/proto
	// response message: number:42
}
