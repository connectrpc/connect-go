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

package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// pingServer implements pingv1connect.PingServiceHandler. Embedding
// UnimplementedPingServiceHandler returns CodeUnimplemented from any
// method the implementation does not define, keeping it forward
// compatible as the service schema grows.
type pingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (pingServer) Ping(ctx context.Context, req *v1.PingRequest) (*v1.PingResponse, error) {
	return &v1.PingResponse{Number: req.Number, Text: req.Text}, nil
}

func (pingServer) Fail(ctx context.Context, req *v1.FailRequest) (*v1.FailResponse, error) {
	return &v1.FailResponse{}, nil
}

func (pingServer) Sum(ctx context.Context, stream pingv1connect.PingServiceSumServerStream) (*v1.SumResponse, error) {
	var total int64
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		total += req.Number
	}
	return &v1.SumResponse{Sum: total}, nil
}

func (pingServer) CountUp(ctx context.Context, req *v1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
	for i := int64(1); i <= req.Number; i++ {
		if err := stream.Send(&v1.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
	return nil
}

func (pingServer) CumSum(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	var total int64
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		total += req.Number
		if err := stream.Send(&v1.CumSumResponse{Sum: total}); err != nil {
			return err
		}
	}
}

// serverLoggingInterceptor logs RPCs that fail. Interceptors are passed to
// connect.NewServer and run before any payload work.
func serverLoggingInterceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		err := next(ctx, spec, stream)
		if err != nil {
			log.Printf("rpc failed: procedure=%s error=%v", spec.Procedure, err)
		}
		return err
	}
}

func main() {
	server := connect.NewServer(serverLoggingInterceptor)
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})

	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	// For gRPC clients, it is convenient to support HTTP/2 without TLS.
	protocols.SetUnencryptedHTTP2(true)
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
