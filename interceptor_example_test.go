package rerpc_test

import (
	"context"
	"fmt"
	"net/http"

	"google.golang.org/protobuf/proto"

	"github.com/rerpc/rerpc"
	pingpb "github.com/rerpc/rerpc/internal/ping/v1test"
)

func ExampleCallMetadata() {
	logger := rerpc.InterceptorFunc(func(next rerpc.Func) rerpc.Func {
		return rerpc.Func(func(ctx context.Context, req proto.Message) (proto.Message, error) {
			if md, ok := rerpc.CallMeta(ctx); ok {
				fmt.Println("calling", md.Spec.Method)
			}
			return next(ctx, req)
		})
	})
	short := rerpc.ShortCircuit(rerpc.Errorf(rerpc.CodeUnimplemented, "no networking in examples"))
	client := pingpb.NewPingServiceClientReRPC(
		"https://invalid-test-url",
		http.DefaultClient,
		rerpc.Intercept(rerpc.NewChain(logger, short)),
	)
	client.Ping(context.Background(), &pingpb.PingRequest{})

	// Output:
	// calling internal.ping.v1test.PingService.Ping
}

func ExampleChain() {
	outer := rerpc.InterceptorFunc(func(next rerpc.Func) rerpc.Func {
		return rerpc.Func(func(ctx context.Context, req proto.Message) (proto.Message, error) {
			fmt.Println("outer interceptor: before call")
			res, err := next(ctx, req)
			fmt.Println("outer interceptor: after call")
			return res, err
		})
	})
	inner := rerpc.InterceptorFunc(func(next rerpc.Func) rerpc.Func {
		return rerpc.Func(func(ctx context.Context, req proto.Message) (proto.Message, error) {
			fmt.Println("inner interceptor: before call")
			res, err := next(ctx, req)
			fmt.Println("inner interceptor: after call")
			return res, err
		})
	})
	short := rerpc.ShortCircuit(rerpc.Errorf(rerpc.CodeUnimplemented, "no networking in examples"))
	client := pingpb.NewPingServiceClientReRPC(
		"https://invalid-test-url",
		http.DefaultClient,
		rerpc.Intercept(rerpc.NewChain(outer, inner, short)),
	)
	client.Ping(context.Background(), &pingpb.PingRequest{})

	// Output:
	// outer interceptor: before call
	// inner interceptor: before call
	// inner interceptor: after call
	// outer interceptor: after call
}
