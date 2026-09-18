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

// Command client calls the ping service through a hybrid transport: streaming
// RPCs travel over WebSocket, unary RPCs fall through to plain HTTP.
package main

import (
	"context"
	"errors"
	"io"
	"log"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

const serverURL = "http://localhost:8080"

// transportInterceptor logs the wire protocol the transport resolved for each
// call, which is how this example shows the routing actually happening.
func transportInterceptor(next connect.ClientFunc) connect.ClientFunc {
	return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
		stream, err := next(ctx, spec)
		if info, ok := connect.CallInfoForClientContext(ctx); ok {
			log.Printf("called %s over %s", spec.Procedure, info.Protocol)
		}
		return stream, err
	}
}

func main() {
	ctx := context.Background()

	// One transport, two wires: streaming RPCs go over WebSocket and everything
	// else over the HTTP transport this builds for itself. Swap the routing
	// rule with connectwebsocket.WithSelector.
	transport, err := connectwebsocket.NewTransport(serverURL)
	if err != nil {
		log.Fatalf("websocket transport: %v", err)
	}
	pingClient := pingv1connect.NewPingServiceClient(
		connect.NewClient(transport, transportInterceptor),
	)

	// Unary: falls through to HTTP.
	res, err := pingClient.Ping(ctx, &v1.PingRequest{Number: 42, Text: "hello"})
	if err != nil {
		log.Fatalf("Ping: %v", err)
	}
	log.Printf("Ping: number=%d text=%q", res.Number, res.Text)

	// Bidirectional stream: travels over WebSocket. Sends and receives
	// interleave, which is the shape HTTP/1.1 cannot carry.
	stream, err := pingClient.CumSum(ctx)
	if err != nil {
		log.Fatalf("CumSum: %v", err)
	}
	for _, number := range []int64{1, 2, 3, 4} {
		if err := stream.Send(&v1.CumSumRequest{Number: number}); err != nil {
			log.Fatalf("CumSum.Send: %v", err)
		}
		msg, err := stream.Receive()
		if err != nil {
			log.Fatalf("CumSum.Receive: %v", err)
		}
		log.Printf("CumSum: sent=%d running total=%d", number, msg.Sum)
	}
	if err := stream.CloseSend(); err != nil {
		log.Fatalf("CumSum.CloseSend: %v", err)
	}
	if _, err := stream.Receive(); !errors.Is(err, io.EOF) {
		log.Fatalf("CumSum: expected io.EOF, got %v", err)
	}
	if err := stream.Close(); err != nil {
		log.Fatalf("CumSum.Close: %v", err)
	}
}
