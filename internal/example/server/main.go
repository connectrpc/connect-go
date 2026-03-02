// Copyright 2021-2025 The Connect Authors
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

	"connectrpc.com/connect"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/simple/connect/ping/v1/pingv1connect"
	"connectrpc.com/validate"
)

type PingServer struct {
	pingv1connect.UnimplementedPingServiceHandler // returns errors from all methods
}

func (ps *PingServer) Ping(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	return &pingv1.PingResponse{
		Number: req.Number,
	}, nil
}

func main() {
	mux := http.NewServeMux()
	// The generated constructors return a path and a plain net/http
	// handler.
	mux.Handle(
		pingv1connect.NewPingServiceHandler(
			&PingServer{},
			// Validation via Protovalidate is almost always recommended
			connect.WithInterceptors(validate.NewInterceptor()),
		),
	)
	p := new(http.Protocols)
	p.SetHTTP1(true)
	// For gRPC clients, it's convenient to support HTTP/2 without TLS.
	p.SetUnencryptedHTTP2(true)
	s := &http.Server{
		Addr:      "localhost:8080",
		Handler:   mux,
		Protocols: p,
	}
	if err := s.ListenAndServe(); err != nil {
		log.Fatalf("listen failed: %v", err)
	}
}
