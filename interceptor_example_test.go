package connect_test

import (
	"context"
	"log"
	"os"

	"github.com/bufconnect/connect"
	pingrpc "github.com/bufconnect/connect/internal/gen/proto/go-connect/connect/ping/v1test"
	pingpb "github.com/bufconnect/connect/internal/gen/proto/go/connect/ping/v1test"
)

func ExampleInterceptor() {
	logger := log.New(os.Stdout, "" /* prefix */, 0 /* flags */)

	loggingInterceptor := connect.UnaryInterceptorFunc(func(next connect.Func) connect.Func {
		return connect.Func(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			logger.Println("calling:", req.Spec().Procedure)
			logger.Println("request:", req.Any())
			res, err := next(ctx, req)
			logger.Println("response:", res.Any())
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
	client.Ping(context.Background(), connect.NewRequest(&pingpb.PingRequest{Number: 42}))

	// Output:
	// calling: connect.ping.v1test.PingService/Ping
	// request: number:42
	// response: number:42
}

func ExampleWithInterceptors() {
	logger := log.New(os.Stdout, "" /* prefix */, 0 /* flags */)
	outer := connect.UnaryInterceptorFunc(func(next connect.Func) connect.Func {
		return connect.Func(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			logger.Println("outer interceptor: before call")
			res, err := next(ctx, req)
			logger.Println("outer interceptor: after call")
			return res, err
		})
	})
	inner := connect.UnaryInterceptorFunc(func(next connect.Func) connect.Func {
		return connect.Func(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
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
	client.Ping(context.Background(), connect.NewRequest(&pingpb.PingRequest{}))

	// Output:
	// outer interceptor: before call
	// inner interceptor: before call
	// inner interceptor: after call
	// outer interceptor: after call
}
