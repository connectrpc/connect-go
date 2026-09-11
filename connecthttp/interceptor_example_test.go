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

package connecthttp_test

import (
	"context"
	"log"
	"os"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

func Example_clientInterceptor() {
	logger := log.New(os.Stdout, "" /* prefix */, 0 /* flags */)
	loggingInterceptor := func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			logger.Println("calling:", spec.Procedure)
			return next(ctx, spec)
		}
	}
	client := pingv1connect.NewPingServiceClient(
		connect.NewClient(connecthttp.NewTransport(examplePingServer.Client(), examplePingServer.URL()), loggingInterceptor),
	)
	if _, err := client.Ping(context.Background(), &pingv1.PingRequest{Number: 42}); err != nil {
		logger.Println("error:", err)
		return
	}

	// Output:
	// calling: /connect.ping.v1.PingService/Ping
}

func Example_interceptors() {
	logger := log.New(os.Stdout, "" /* prefix */, 0 /* flags */)
	logInterceptor := func(name string) connect.ClientInterceptor {
		return func(next connect.ClientFunc) connect.ClientFunc {
			return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
				logger.Printf("%s interceptor: before call", name)
				stream, err := next(ctx, spec)
				if err != nil {
					return nil, err
				}
				return &loggingClientStream{ClientStream: stream, name: name, logger: logger}, nil
			}
		}
	}
	client := pingv1connect.NewPingServiceClient(
		connect.NewClient(connecthttp.NewTransport(examplePingServer.Client(), examplePingServer.URL()), logInterceptor("outer"), logInterceptor("inner")),
	)
	if _, err := client.Ping(context.Background(), &pingv1.PingRequest{}); err != nil {
		logger.Println("error:", err)
		return
	}

	// Output:
	// outer interceptor: before call
	// inner interceptor: before call
	// inner interceptor: after call
	// outer interceptor: after call
}

type loggingClientStream struct {
	connect.ClientStream

	name   string
	logger *log.Logger
}

func (s *loggingClientStream) Close() error {
	err := s.ClientStream.Close()
	s.logger.Printf("%s interceptor: after call", s.name)
	return err
}
