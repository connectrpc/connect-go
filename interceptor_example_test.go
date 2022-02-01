package connect_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/bufconnect/connect"
	pingrpc "github.com/bufconnect/connect/internal/gen/proto/go-connect/connect/ping/v1test"
	pingpb "github.com/bufconnect/connect/internal/gen/proto/go/connect/ping/v1test"
)

func ExampleInterceptor() {
	logger := log.New(os.Stdout, "" /* prefix */, 0 /* flags */)
	logProcedure := connect.UnaryInterceptorFunc(func(next connect.Func) connect.Func {
		return connect.Func(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			fmt.Println("calling", req.Spec().Procedure)
			return next(ctx, req)
		})
	})
	// This interceptor prevents the client from making network requests in
	// examples. Leave it out in real code!
	short := ShortCircuit(connect.Errorf(connect.CodeUnimplemented, "no networking in examples"))
	client, err := pingrpc.NewPingServiceClient(
		"https://invalid-test-url",
		http.DefaultClient,
		connect.Interceptors(logProcedure, short),
	)
	if err != nil {
		logger.Print("Error: ", err)
		return
	}
	client.Ping(context.Background(), &pingpb.PingRequest{})

	// Output:
	// calling connect.ping.v1test.PingService/Ping
}

func ExampleInterceptors() {
	logger := log.New(os.Stdout, "" /* prefix */, 0 /* flags */)
	outer := connect.UnaryInterceptorFunc(func(next connect.Func) connect.Func {
		return connect.Func(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			fmt.Println("outer interceptor: before call")
			res, err := next(ctx, req)
			fmt.Println("outer interceptor: after call")
			return res, err
		})
	})
	inner := connect.UnaryInterceptorFunc(func(next connect.Func) connect.Func {
		return connect.Func(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			fmt.Println("inner interceptor: before call")
			res, err := next(ctx, req)
			fmt.Println("inner interceptor: after call")
			return res, err
		})
	})
	// This interceptor prevents the client from making network requests in
	// examples. Leave it out in real code!
	short := ShortCircuit(connect.Errorf(connect.CodeUnimplemented, "no networking in examples"))
	client, err := pingrpc.NewPingServiceClient(
		"https://invalid-test-url",
		http.DefaultClient,
		connect.Interceptors(outer, inner, short),
	)
	if err != nil {
		logger.Print("Error: ", err)
		return
	}
	client.Ping(context.Background(), &pingpb.PingRequest{})

	// Output:
	// outer interceptor: before call
	// inner interceptor: before call
	// inner interceptor: after call
	// outer interceptor: after call
}
