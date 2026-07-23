// Copyright 2021-2026 The Connect Authors
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

package connecthttp_test

import (
	"context"
	"errors"
	"io"
	"net/http"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// ExamplePingServer implements some trivial business logic. The Protobuf
// definition for this API is in proto/connect/ping/v1/ping.proto.
type ExamplePingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

// Ping implements pingv1connect.PingServiceHandler.
func (*ExamplePingServer) Ping(
	_ context.Context,
	request *pingv1.PingRequest,
) (*pingv1.PingResponse, error) {
	return &pingv1.PingResponse{
		Number: request.GetNumber(),
		Text:   request.GetText(),
	}, nil
}

// Sum implements pingv1connect.PingServiceHandler.
func (p *ExamplePingServer) Sum(_ context.Context, stream pingv1connect.PingServiceSumServerStream) (*pingv1.SumResponse, error) {
	var sum int64
	for {
		msg, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		sum += msg.GetNumber()
	}

	return &pingv1.SumResponse{Sum: sum}, nil
}

// CountUp implements pingv1connect.PingServiceHandler.
func (p *ExamplePingServer) CountUp(_ context.Context, request *pingv1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
	for number := int64(1); number <= request.GetNumber(); number++ {
		if err := stream.Send(&pingv1.CountUpResponse{Number: number}); err != nil {
			return err
		}
	}
	return nil
}

// CumSum implements pingv1connect.PingServiceHandler.
func (p *ExamplePingServer) CumSum(_ context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	var sum int64
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		sum += msg.GetNumber()
		if err := stream.Send(&pingv1.CumSumResponse{Sum: sum}); err != nil {
			return err
		}
	}
}

func Example_handler() {
	// protoc-gen-connect-go generates constructors that return plain net/http
	// Handlers, so they're compatible with most Go HTTP routers and middleware
	// (for example, net/http's StripPrefix). Each handler automatically supports
	// the Connect, gRPC, and gRPC-Web protocols.
	mux := http.NewServeMux()
	server := connect.NewServer()

	pingv1connect.RegisterPingServiceHandler(server, &ExamplePingServer{})
	connecthttp. // our business logic
			Mount(mux, server)

	// You can serve gRPC's health and server reflection APIs using
	// connectrpc.com/grpchealth and connectrpc.com/grpcreflect.
	_ = http.ListenAndServeTLS(
		"localhost:8080",
		"internal/testdata/server.crt",
		"internal/testdata/server.key",
		mux,
	)
	// To serve HTTP/2 requests without TLS (as many gRPC clients expect), use
	// Protocols.SetUnencryptedHTTP2 and change to:
	// p := new(http.Protocols)
	// p.SetHTTP1(true)
	// p.SetUnencryptedHTTP2(true)
	// s := &http.Server{
	// 	Addr:      "localhost:8080",
	// 	Handler:   mux,
	// 	Protocols: p,
	// }
	// _ = s.ListenAndServe()
}
