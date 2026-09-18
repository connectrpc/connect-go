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

// Command server serves the ping service over two transports at once: unary
// RPCs on plain HTTP, and streaming RPCs over WebSocket, sharing one set of
// procedure paths.
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// pingServer implements the two methods this example exercises: Ping, a unary
// request/response, and CumSum, a bidirectional stream.
type pingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (pingServer) Sum(ctx context.Context, req pingv1connect.PingServiceSumServerStream) (*v1.SumResponse, error) {
	var total int64
	for {
		req, err := req.Receive()
		if errors.Is(err, io.EOF) {
			return &v1.SumResponse{Sum: total}, nil
		}
		if err != nil {
			return nil, err
		}
		total += req.Number
	}
}

// transportInterceptor logs which wire protocol carried each RPC, which is how
// this example shows the routing actually happening.
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
	server := connect.NewServer(transportInterceptor)
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})

	mux := http.NewServeMux()
	// One call registers every procedure over both transports at the same
	// paths: a WebSocket upgrade is served by connectwebsocket, and every
	// other request by the connecthttp handler underneath. The client picks
	// which transport carries each RPC; the server accepts either.
	connectwebsocket.Mount(mux, server)

	protocols := new(http.Protocols)
	// HTTP/1.1 only: a WebSocket upgrade needs a hijackable connection, which
	// HTTP/2 does not provide.
	protocols.SetHTTP1(true)
	httpServer := &http.Server{
		Addr:      "localhost:8080",
		Handler:   mux,
		Protocols: protocols,
	}
	log.Println("listening on", httpServer.Addr)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
