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

// Command client misframes on purpose, to show what the server's protocol
// error handler sees.
//
// It speaks the wire format by hand rather than through connectwebsocket,
// because the Go client cannot produce these mistakes: the transport frames
// correctly. The clients a monitor actually catches are hand-written ones —
// a browser implementation, or another language's — which is exactly what this
// stands in for.
//
// Each fault ends its connection, so every case gets a fresh one. Run the
// matching server first.
package main

import (
	"context"
	"flag"
	"log"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

// The message markers, one BMP scalar each.
const (
	markerBody            = 'B'
	markerMetadata        = 'M'
	markerServerEndStream = 'S'
)

func main() {
	serverURL := flag.String("url", "http://localhost:8080", "base URL of the server")
	flag.Parse()

	message, err := proto.Marshal(&v1.CumSumRequest{Number: 7})
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}

	cases := []struct {
		name  string
		send  func(context.Context, *websocket.Conn) error
		which string
	}{
		{
			name:  "a well-framed message",
			which: "no fault: the monitor stays silent",
			send: func(ctx context.Context, conn *websocket.Conn) error {
				return writeMessage(ctx, conn, false, markerBody, message)
			},
		},
		{
			name:  "a marker nobody has defined",
			which: "marker",
			send: func(ctx context.Context, conn *websocket.Conn) error {
				return writeMessage(ctx, conn, false, 'Z', message)
			},
		},
		{
			// Repeated on purpose: the server counts per client, so this is
			// what a consistently broken implementation looks like in its log.
			name:  "the same unknown marker again",
			which: "marker",
			send: func(ctx context.Context, conn *websocket.Conn) error {
				return writeMessage(ctx, conn, false, 'Z', message)
			},
		},
		{
			name:  "the server's own end-of-stream marker",
			which: "marker",
			send: func(ctx context.Context, conn *websocket.Conn) error {
				return writeMessage(ctx, conn, true, markerServerEndStream, []byte("{}"))
			},
		},
		{
			name:  "an empty body in a text frame",
			which: "frame_type",
			send: func(ctx context.Context, conn *websocket.Conn) error {
				// An empty JSON message is {}, never zero bytes.
				return writeMessage(ctx, conn, true, markerBody, nil)
			},
		},
		{
			name:  "a second M message",
			which: "metadata",
			send: func(ctx context.Context, conn *websocket.Conn) error {
				return writeMessage(ctx, conn, true, markerMetadata, []byte("{}"))
			},
		},
	}

	for _, test := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		reply, err := exchange(ctx, *serverURL, test.send)
		cancel()
		if err != nil {
			log.Printf("%-38s -> %-16s connection ended: %v", test.name, test.which, err)
			continue
		}
		log.Printf("%-38s -> %-16s server replied: %s", test.name, test.which, reply)
	}
	log.Println("check the server's log: every fault above was reported to its handler")

	// The mirror image: an ordinary client, talking through connectwebsocket,
	// watching for a *server* that frames its responses wrongly. Against the
	// server above this stays silent; against one started with -misframe it
	// reports what arrived. Run both ways to see each half.
	log.Println("---")
	watchForServerFaults(*serverURL)
}

// watchForServerFaults makes one ordinary RPC with a client-side protocol
// error handler registered, which is the only way a client learns that a
// server misframed rather than simply failed.
func watchForServerFaults(serverURL string) {
	transport, err := connectwebsocket.NewTransport(
		serverURL,
		connectwebsocket.WithClientProtocolErrorHandler(
			func(spec connect.Spec, fault connectwebsocket.ProtocolFault, err *connect.Error) {
				log.Printf("the server misframed a response to %s: %s — %v",
					spec.Procedure, fault, err)
			},
		),
	)
	if err != nil {
		log.Fatalf("websocket transport: %v", err)
	}
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.CumSum(ctx)
	if err != nil {
		log.Fatalf("CumSum: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if err := stream.Send(&v1.CumSumRequest{Number: 7}); err != nil {
		log.Printf("Send: %v", err)
		return
	}
	response, err := stream.Receive()
	if err != nil {
		log.Printf("Receive failed: %v (the handler above says why, if it was a framing fault)", err)
		return
	}
	log.Printf("server framed its response correctly: Sum=%d", response.Sum)
}

// exchange opens one connection, sends one thing, and reports what came back.
func exchange(
	ctx context.Context,
	serverURL string,
	send func(context.Context, *websocket.Conn) error,
) (string, error) {
	url := "ws" + strings.TrimPrefix(serverURL, "http") + pingv1connect.PingServiceCumSumProcedure
	conn, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols: []string{"connectrpc.1+proto"},
	})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.CloseNow() }()

	// Every stream opens with exactly one M message, empty when the client has
	// none. Anything else first is refused before the RPC starts.
	if err := writeMessage(ctx, conn, true, markerMetadata, []byte("{}")); err != nil {
		return "", err
	}
	if err := send(ctx, conn); err != nil {
		return "", err
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", nil
	}
	// S marks the end-of-stream message, whose payload is the JSON verdict;
	// anything else is a normal response message.
	if rune(data[0]) == markerServerEndStream {
		return string(data[1:]), nil
	}
	return "a response message", nil
}

// writeMessage frames one message with an arbitrary marker and frame type,
// which is how this client produces mistakes the transport would not.
func writeMessage(
	ctx context.Context,
	conn *websocket.Conn,
	text bool,
	marker rune,
	payload []byte,
) error {
	frame := append([]byte(string(marker)), payload...)
	messageType := websocket.MessageBinary
	if text {
		messageType = websocket.MessageText
	}
	return conn.Write(ctx, messageType, frame)
}
