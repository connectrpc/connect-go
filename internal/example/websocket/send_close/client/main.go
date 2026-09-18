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
	"fmt"
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

	ctx := context.Background()
	stream, err := pingClient.Sum(ctx)
	if err != nil {
		log.Fatalf("Sum: %v", err)
	}
	for i := range 10 {
		err := stream.Send(&v1.SumRequest{Number: int64(i)})
		if err != nil {
			log.Fatalf("Sum.Send: %v", err)
		}
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		log.Fatalf("Sum.CloseSend: %v", err)
	}
	fmt.Println(resp.Sum)
}
