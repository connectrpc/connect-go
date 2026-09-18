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

// Command client runs the same server stream four times against one server,
// choosing a different transport each time. Nothing about the server changes
// between runs: the Selector is a client-side decision, and the server accepts
// whichever wire the client picked.
//
// Run the transport_choice server first, then:
//
//	go run ./websocket/transport_choice/client
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// choices are the routing rules this example demonstrates, in the order it
// runs them.
var choices = []struct {
	name     string
	selector connectwebsocket.Selector
	explain  string
}{
	{
		name:    "default",
		explain: "streaming over WebSocket, unary over HTTP",
		// Leaving the selector nil keeps SelectStreaming, the default.
	},
	{
		name:     "all",
		selector: connectwebsocket.SelectAll,
		explain:  "everything over WebSocket, including unary",
	},
	{
		name:     "bidi only",
		selector: connectwebsocket.SelectBidi,
		explain:  "only full-duplex RPCs upgrade; a server stream is plain HTTP",
	},
	{
		name:     "never",
		selector: func(connect.Spec) bool { return false },
		explain:  "no upgrade at all; the fallback transport carries everything",
	},
}

func main() {
	serverURL := flag.String("url", "http://localhost:8080", "base URL of the server")
	count := flag.Int64("count", 3, "how many numbers to ask CountUp for")
	flag.Parse()

	for _, choice := range choices {
		options := []connectwebsocket.ClientOption{}
		if choice.selector != nil {
			options = append(options, connectwebsocket.WithSelector(choice.selector))
		}
		transport, err := connectwebsocket.NewTransport(*serverURL, options...)
		if err != nil {
			log.Fatalf("%s: websocket transport: %v", choice.name, err)
		}

		var protocol string
		client := pingv1connect.NewPingServiceClient(
			connect.NewClient(transport, recordProtocol(&protocol)),
		)
		numbers, err := countUp(context.Background(), client, *count)
		if err != nil {
			log.Fatalf("%s: CountUp: %v", choice.name, err)
		}
		log.Printf("selector %-9s -> CountUp over %-18s got %v (%s)",
			choice.name, protocol, numbers, choice.explain)
	}
}

// countUp drains the server stream and returns what it produced.
func countUp(
	ctx context.Context,
	client pingv1connect.PingServiceClient,
	count int64,
) ([]int64, error) {
	stream, err := client.CountUp(ctx, &v1.CountUpRequest{Number: count})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := stream.Close(); err != nil {
			log.Printf("CountUp.Close: %v", err)
		}
	}()
	var numbers []int64
	for {
		message, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			// The end of the stream, not a failure.
			return numbers, nil
		}
		if err != nil {
			return nil, err
		}
		numbers = append(numbers, message.Number)
	}
}

// recordProtocol captures the wire the transport resolved for each RPC, which
// is the only way to observe the selector's decision from the client side.
func recordProtocol(into *string) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			stream, err := next(ctx, spec)
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				*into = info.Protocol
			}
			return stream, err
		}
	}
}
