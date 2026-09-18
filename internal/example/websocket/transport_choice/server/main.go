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

// Command server serves one streaming procedure over both transports, so that
// the client alone decides which wire carries it. The server makes no such
// choice: it answers a WebSocket upgrade and a plain HTTP request on the same
// path, and logs which one arrived.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// pingServer implements a unary procedure and a server stream. A server stream
// is the interesting shape here: unlike a bidirectional one it works over
// either transport, so the client's choice is a real choice rather than a
// requirement.
type pingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (pingServer) Ping(_ context.Context, req *v1.PingRequest) (*v1.PingResponse, error) {
	return &v1.PingResponse{Number: req.Number, Text: req.Text}, nil
}

func (pingServer) CountUp(
	_ context.Context,
	req *v1.CountUpRequest,
	stream pingv1connect.PingServiceCountUpServerStream,
) error {
	for i := int64(1); i <= req.Number; i++ {
		if err := stream.Send(&v1.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
	return nil
}

// transportInterceptor logs the wire each RPC arrived on, which is how this
// example shows the client's choice taking effect.
func transportInterceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		protocol := "unknown"
		if info, ok := connect.CallInfoForServerContext(ctx); ok {
			protocol = info.Protocol
		}
		log.Printf("serving %s over %s", spec.Procedure, protocol)
		return next(ctx, spec, stream)
	}
}

func main() {
	addr := flag.String("addr", "localhost:8080", "address to listen on")
	flag.Parse()

	server := connect.NewServer(transportInterceptor)
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})

	mux := http.NewServeMux()
	// Every procedure is registered over both transports. There is deliberately
	// no server-side selector: which transport carries an RPC is the client's
	// decision, and a server that served only one would reject a client that
	// chose the other.
	connectwebsocket.Mount(mux, server)

	protocols := new(http.Protocols)
	// HTTP/1.1 only: a WebSocket upgrade needs a hijackable connection, which
	// HTTP/2 does not provide.
	protocols.SetHTTP1(true)
	httpServer := &http.Server{
		Addr:      *addr,
		Handler:   mux,
		Protocols: protocols,
	}
	log.Println("listening on", httpServer.Addr)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
