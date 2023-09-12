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
	"errors"
	"io"
	"net/http"

	connect "connectrpc.com/connect"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect"
)

// ExamplePingServer implements some trivial business logic. The Protobuf
// definition for this API is in proto/connect/ping/v1/ping.proto.
type ExamplePingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

// Ping implements pingv1connect.PingServiceHandler.
func (*ExamplePingServer) Ping(
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

// Sum implements pingv1connect.PingServiceHandler.
func (p *ExamplePingServer) Sum(ctx context.Context, stream *connect.ClientStream[pingv1.SumRequest]) (*connect.Response[pingv1.SumResponse], error) {
	var sum int64
	for stream.Receive() {
		sum += stream.Msg().Number
	}
	if stream.Err() != nil {
		return nil, stream.Err()
	}
	response := connect.NewResponse(&pingv1.SumResponse{Sum: sum})
	return response, nil
}

// CountUp implements pingv1connect.PingServiceHandler.
func (p *ExamplePingServer) CountUp(ctx context.Context, request *connect.Request[pingv1.CountUpRequest], stream *connect.ServerStream[pingv1.CountUpResponse]) error {
	for number := int64(1); number <= request.Msg.Number; number++ {
		if err := stream.Send(&pingv1.CountUpResponse{Number: number}); err != nil {
			return err
		}
	}
	return nil
}

// CumSum implements pingv1connect.PingServiceHandler.
func (p *ExamplePingServer) CumSum(ctx context.Context, stream *connect.BidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse]) error {
	var sum int64
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		sum += msg.Number
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
	mux.Handle(
		pingv1connect.NewPingServiceHandler(
			&ExamplePingServer{}, // our business logic
		),
	)
	// You can serve gRPC's health and server reflection APIs using
	// connectrpc.com/grpchealth and connectrpc.com/grpcreflect.
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
