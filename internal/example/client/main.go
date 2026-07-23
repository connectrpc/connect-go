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

// clientLoggingInterceptor logs each call before the stream is opened.
// Interceptors are passed to connect.NewClient and run in argument order.
func clientLoggingInterceptor(next connect.ClientFunc) connect.ClientFunc {
	return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
		log.Printf("calling %s", spec.Procedure)
		return next(ctx, spec)
	}
}

func main() {
	ctx := context.Background()
	client := connect.NewClient(
		connecthttp.NewTransport(http.DefaultClient, "http://localhost:8080"),
		clientLoggingInterceptor,
	)
	pingClient := pingv1connect.NewPingServiceClient(client)

	res, err := pingClient.Ping(ctx, &v1.PingRequest{Number: 42, Text: "hello"})
	if err != nil {
		log.Fatalf("Ping: %v", err)
	}
	log.Printf("Ping: number=%d text=%q", res.Number, res.Text)

	stream, err := pingClient.CountUp(ctx, &v1.CountUpRequest{Number: 3})
	if err != nil {
		log.Fatalf("CountUp: %v", err)
	}
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			log.Fatalf("CountUp.Receive: %v", err)
		}
		log.Printf("CountUp: %d", msg.Number)
	}
}
