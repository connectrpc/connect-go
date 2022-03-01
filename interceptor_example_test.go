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

package connect_test

import (
	"context"
	"log"
	"os"

	"github.com/bufbuild/connect"
	pingrpc "github.com/bufbuild/connect/internal/gen/proto/connect/connect/ping/v1test"
	pingpb "github.com/bufbuild/connect/internal/gen/proto/go/connect/ping/v1test"
)

func ExampleInterceptor() {
	logger := log.New(os.Stdout, "" /* prefix */, 0 /* flags */)

	loggingInterceptor := connect.UnaryInterceptorFunc(func(next connect.Func) connect.Func {
		return connect.Func(func(ctx context.Context, req connect.AnyEnvelope) (connect.AnyEnvelope, error) {
			logger.Println("calling:", req.Spec().Procedure)
			logger.Println("request:", req.Any())
			res, err := next(ctx, req)
			if err != nil {
				logger.Println("error:", err)
			} else {
				logger.Println("response:", res.Any())
			}
			return res, err
		})
	})

	client, err := pingrpc.NewPingServiceClient(
		examplePingServer.URL(),
		examplePingServer.Client(),
		connect.WithInterceptors(loggingInterceptor),
	)
	if err != nil {
		logger.Println("error:", err)
		return
	}
	client.Ping(context.Background(), connect.NewEnvelope(&pingpb.PingRequest{Number: 42}))

	// Output:
	// calling: connect.ping.v1test.PingService/Ping
	// request: number:42
	// response: number:42
}

func ExampleWithInterceptors() {
	logger := log.New(os.Stdout, "" /* prefix */, 0 /* flags */)
	outer := connect.UnaryInterceptorFunc(func(next connect.Func) connect.Func {
		return connect.Func(func(ctx context.Context, req connect.AnyEnvelope) (connect.AnyEnvelope, error) {
			logger.Println("outer interceptor: before call")
			res, err := next(ctx, req)
			logger.Println("outer interceptor: after call")
			return res, err
		})
	})
	inner := connect.UnaryInterceptorFunc(func(next connect.Func) connect.Func {
		return connect.Func(func(ctx context.Context, req connect.AnyEnvelope) (connect.AnyEnvelope, error) {
			logger.Println("inner interceptor: before call")
			res, err := next(ctx, req)
			logger.Println("inner interceptor: after call")
			return res, err
		})
	})
	client, err := pingrpc.NewPingServiceClient(
		examplePingServer.URL(),
		examplePingServer.Client(),
		connect.WithInterceptors(outer, inner),
	)
	if err != nil {
		logger.Println("error:", err)
		return
	}
	client.Ping(context.Background(), connect.NewEnvelope(&pingpb.PingRequest{}))

	// Output:
	// outer interceptor: before call
	// inner interceptor: before call
	// inner interceptor: after call
	// outer interceptor: after call
}
