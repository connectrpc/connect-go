// Copyright 2021-2022 Buf Technologies, Inc.
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

package main

import (
	"context"
	"log"
	"net/http"

	"github.com/bufbuild/connect-go"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
	"github.com/bufbuild/connect-go/internal/gen/connect/ping/v1/pingv1connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type PingServer struct {
	// Returns errors from all methods we don't want to implement for now.
	pingv1connect.UnimplementedPingServiceHandler
}

func (ps *PingServer) Ping(
	_ context.Context,
	req *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {

	// Finally `connect.Request` behaves like standard HTTP library.
	// For example, it gives you direct access to headers and trailers.
	log.Println("Request header:", req.Header().Get("Some-Header"))

	// The request for this method is a strongly-typed as defined by *pingv1.PingRequest,
	// so we can access for example `Msg` field safely!
	res := connect.NewResponse(&pingv1.PingResponse{Number: req.Msg.Number})

	// Similarly we can add extra headers to response.
	res.Header().Set("Some-Other-Header", "hello!")

	// Just return response or error like you would do in non-remote Go method!
	return res, nil
}

func main() {
	// Standard mux for handlers.
	mux := http.NewServeMux()

	// Register handlers for gRPC connect ping service we implemented above.
	mux.Handle(pingv1connect.NewPingServiceHandler(&PingServer{}))

	log.Println("Starting server that accepts both Connect enabled HTTP Post, gRPC and gRPC-Web!")
	if err := http.ListenAndServe(
		"localhost:8080",
		// For the example gRPC clients, it's convenient to support HTTP/2 without TLS.
		h2c.NewHandler(mux, &http2.Server{}),
	); err != nil {
		log.Fatal(err)
	}
}
