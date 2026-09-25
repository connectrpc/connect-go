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

// Command client sums ten numbers over a client-streaming RPC.
//
// Run with -abandon to drop the connection halfway instead of ending the
// stream properly. The two look identical to the client's Send loop and
// different to the server, which is the point: a handler that treats a dead
// client as a finished one commits a total the client never asked for.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

const serverURL = "http://localhost:8080"

// abandon is set from the command line; see the package comment.
var abandon *bool

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
	abandon = flag.Bool("abandon", false,
		"drop the connection mid-stream instead of sending end-of-stream")
	flag.Parse()

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

	// NewClientContext attaches the CallInfo the transport publishes response
	// metadata onto; without it there is nowhere for the server's M message to
	// land.
	ctx, info := connect.NewClientContext(context.Background())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := pingClient.Sum(ctx)
	if err != nil {
		log.Fatalf("Sum: %v", err)
	}
	for i := range 10 {
		if err := stream.Send(&v1.SumRequest{Number: int64(i)}); err != nil {
			log.Fatalf("Sum.Send: %v", err)
		}
		if *abandon && i == 4 {
			// Walk away mid-stream, the way a crashed or disconnected client
			// does: cancel and return without closing the stream. Nothing
			// sends a C message, so the server learns the stream ended
			// without learning that the client was finished.
			//
			// Deliberately not CloseAndReceive here. That sends a C, and
			// whether the cancelled context stops it in time is a race — so
			// the server would see a finished client on some runs and an
			// abandoned one on others.
			log.Println("abandoning the stream after 5 of 10 values")
			cancel()
			log.Println("check the server's log: it discarded the partial sum rather than answering")
			return
		}
	}
	response, err := stream.CloseAndReceive()
	if err != nil {
		log.Fatalf("Sum.CloseAndReceive: %v", err)
	}
	// Response metadata the handler set, carried in the M message that opens
	// every response stream. Over plain HTTP the same value would arrive in
	// the response header block.
	log.Printf("server counted %s values", info.ResponseHeader().Get("Acme-Values-Counted"))
	fmt.Println(response.Sum)
}
