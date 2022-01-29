// Code generated by protoc-gen-go-rerpc. DO NOT EDIT.
// versions:
// - protoc-gen-go-rerpc v0.0.1
// - protoc              v3.17.3
// source: rerpc/ping/v1test/ping.proto

package pingv1test

import (
	context "context"
	errors "errors"
	strings "strings"

	rerpc "github.com/rerpc/rerpc"
	clientstream "github.com/rerpc/rerpc/clientstream"
	protobuf "github.com/rerpc/rerpc/codec/protobuf"
	handlerstream "github.com/rerpc/rerpc/handlerstream"
	v1test "github.com/rerpc/rerpc/internal/gen/proto/go/rerpc/ping/v1test"
)

// This is a compile-time assertion to ensure that this generated file and the
// rerpc package are compatible. If you get a compiler error that this constant
// isn't defined, this code was generated with a version of rerpc newer than the
// one compiled into your binary. You can fix the problem by either regenerating
// this code with an older version of rerpc or updating the rerpc version
// compiled into your binary.
const _ = rerpc.SupportsCodeGenV0 // requires reRPC v0.0.1 or later

// WrappedPingServiceClient is a client for the rerpc.ping.v1test.PingService
// service.
//
// It's a simplified wrapper around the full-featured API of
// UnwrappedPingServiceClient.
type WrappedPingServiceClient interface {
	Ping(context.Context, *v1test.PingRequest) (*v1test.PingResponse, error)
	Fail(context.Context, *v1test.FailRequest) (*v1test.FailResponse, error)
	Sum(context.Context) *clientstream.Client[v1test.SumRequest, v1test.SumResponse]
	CountUp(context.Context, *v1test.CountUpRequest) (*clientstream.Server[v1test.CountUpResponse], error)
	CumSum(context.Context) *clientstream.Bidirectional[v1test.CumSumRequest, v1test.CumSumResponse]
}

// UnwrappedPingServiceClient is a client for the rerpc.ping.v1test.PingService
// service. It's more complex than WrappedPingServiceClient, but it gives
// callers more fine-grained control (e.g., sending and receiving headers).
type UnwrappedPingServiceClient interface {
	Ping(context.Context, *rerpc.Request[v1test.PingRequest]) (*rerpc.Response[v1test.PingResponse], error)
	Fail(context.Context, *rerpc.Request[v1test.FailRequest]) (*rerpc.Response[v1test.FailResponse], error)
	Sum(context.Context) *clientstream.Client[v1test.SumRequest, v1test.SumResponse]
	CountUp(context.Context, *rerpc.Request[v1test.CountUpRequest]) (*clientstream.Server[v1test.CountUpResponse], error)
	CumSum(context.Context) *clientstream.Bidirectional[v1test.CumSumRequest, v1test.CumSumResponse]
}

// PingServiceClient is a client for the rerpc.ping.v1test.PingService service.
type PingServiceClient struct {
	client unwrappedPingServiceClient
}

var _ WrappedPingServiceClient = (*PingServiceClient)(nil)

// NewPingServiceClient constructs a client for the
// rerpc.ping.v1test.PingService service. By default, it uses the binary
// protobuf codec.
//
// The URL supplied here should be the base URL for the gRPC server (e.g.,
// https://api.acme.com or https://acme.com/grpc).
func NewPingServiceClient(baseURL string, doer rerpc.Doer, opts ...rerpc.ClientOption) (*PingServiceClient, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	opts = append([]rerpc.ClientOption{
		rerpc.Codec(protobuf.NameBinary, protobuf.NewBinary()),
	}, opts...)
	pingFunc, err := rerpc.NewClientFunc[v1test.PingRequest, v1test.PingResponse](
		doer,
		baseURL,
		"rerpc.ping.v1test.PingService/Ping",
		opts...,
	)
	if err != nil {
		return nil, err
	}
	failFunc, err := rerpc.NewClientFunc[v1test.FailRequest, v1test.FailResponse](
		doer,
		baseURL,
		"rerpc.ping.v1test.PingService/Fail",
		opts...,
	)
	if err != nil {
		return nil, err
	}
	sumFunc, err := rerpc.NewClientStream(
		doer,
		rerpc.StreamTypeClient,
		baseURL,
		"rerpc.ping.v1test.PingService/Sum",
		opts...,
	)
	if err != nil {
		return nil, err
	}
	countUpFunc, err := rerpc.NewClientStream(
		doer,
		rerpc.StreamTypeServer,
		baseURL,
		"rerpc.ping.v1test.PingService/CountUp",
		opts...,
	)
	if err != nil {
		return nil, err
	}
	cumSumFunc, err := rerpc.NewClientStream(
		doer,
		rerpc.StreamTypeBidirectional,
		baseURL,
		"rerpc.ping.v1test.PingService/CumSum",
		opts...,
	)
	if err != nil {
		return nil, err
	}
	return &PingServiceClient{client: unwrappedPingServiceClient{
		ping:    pingFunc,
		fail:    failFunc,
		sum:     sumFunc,
		countUp: countUpFunc,
		cumSum:  cumSumFunc,
	}}, nil
}

// Ping calls rerpc.ping.v1test.PingService.Ping.
func (c *PingServiceClient) Ping(ctx context.Context, req *v1test.PingRequest) (*v1test.PingResponse, error) {
	res, err := c.client.Ping(ctx, rerpc.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// Fail calls rerpc.ping.v1test.PingService.Fail.
func (c *PingServiceClient) Fail(ctx context.Context, req *v1test.FailRequest) (*v1test.FailResponse, error) {
	res, err := c.client.Fail(ctx, rerpc.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// Sum calls rerpc.ping.v1test.PingService.Sum.
func (c *PingServiceClient) Sum(ctx context.Context) *clientstream.Client[v1test.SumRequest, v1test.SumResponse] {
	return c.client.Sum(ctx)
}

// CountUp calls rerpc.ping.v1test.PingService.CountUp.
func (c *PingServiceClient) CountUp(ctx context.Context, req *v1test.CountUpRequest) (*clientstream.Server[v1test.CountUpResponse], error) {
	return c.client.CountUp(ctx, rerpc.NewRequest(req))
}

// CumSum calls rerpc.ping.v1test.PingService.CumSum.
func (c *PingServiceClient) CumSum(ctx context.Context) *clientstream.Bidirectional[v1test.CumSumRequest, v1test.CumSumResponse] {
	return c.client.CumSum(ctx)
}

// Unwrap exposes the underlying generic client. Use it if you need finer
// control (e.g., sending and receiving headers).
func (c *PingServiceClient) Unwrap() UnwrappedPingServiceClient {
	return &c.client
}

type unwrappedPingServiceClient struct {
	ping    func(context.Context, *rerpc.Request[v1test.PingRequest]) (*rerpc.Response[v1test.PingResponse], error)
	fail    func(context.Context, *rerpc.Request[v1test.FailRequest]) (*rerpc.Response[v1test.FailResponse], error)
	sum     func(context.Context) (rerpc.Sender, rerpc.Receiver)
	countUp func(context.Context) (rerpc.Sender, rerpc.Receiver)
	cumSum  func(context.Context) (rerpc.Sender, rerpc.Receiver)
}

var _ UnwrappedPingServiceClient = (*unwrappedPingServiceClient)(nil)

// Ping calls rerpc.ping.v1test.PingService.Ping.
func (c *unwrappedPingServiceClient) Ping(ctx context.Context, req *rerpc.Request[v1test.PingRequest]) (*rerpc.Response[v1test.PingResponse], error) {
	return c.ping(ctx, req)
}

// Fail calls rerpc.ping.v1test.PingService.Fail.
func (c *unwrappedPingServiceClient) Fail(ctx context.Context, req *rerpc.Request[v1test.FailRequest]) (*rerpc.Response[v1test.FailResponse], error) {
	return c.fail(ctx, req)
}

// Sum calls rerpc.ping.v1test.PingService.Sum.
func (c *unwrappedPingServiceClient) Sum(ctx context.Context) *clientstream.Client[v1test.SumRequest, v1test.SumResponse] {
	sender, receiver := c.sum(ctx)
	return clientstream.NewClient[v1test.SumRequest, v1test.SumResponse](sender, receiver)
}

// CountUp calls rerpc.ping.v1test.PingService.CountUp.
func (c *unwrappedPingServiceClient) CountUp(ctx context.Context, req *rerpc.Request[v1test.CountUpRequest]) (*clientstream.Server[v1test.CountUpResponse], error) {
	sender, receiver := c.countUp(ctx)
	if err := sender.Send(req.Msg); err != nil {
		_ = sender.Close(err)
		_ = receiver.Close()
		return nil, err
	}
	if err := sender.Close(nil); err != nil {
		_ = receiver.Close()
		return nil, err
	}
	return clientstream.NewServer[v1test.CountUpResponse](receiver), nil
}

// CumSum calls rerpc.ping.v1test.PingService.CumSum.
func (c *unwrappedPingServiceClient) CumSum(ctx context.Context) *clientstream.Bidirectional[v1test.CumSumRequest, v1test.CumSumResponse] {
	sender, receiver := c.cumSum(ctx)
	return clientstream.NewBidirectional[v1test.CumSumRequest, v1test.CumSumResponse](sender, receiver)
}

// PingService is an implementation of the rerpc.ping.v1test.PingService
// service.
//
// When writing your code, you can always implement the complete PingService
// interface. However, if you don't need to work with headers, you can instead
// implement a simpler version of any or all of the unary methods. Where
// available, the simplified signatures are listed in comments.
//
// NewPingService first tries to find the simplified version of each method,
// then falls back to the more complex version. If neither is implemented,
// rerpc.NewServeMux will return an error.
type PingService interface {
	// Can also be implemented in a simplified form:
	// Ping(context.Context, *v1test.PingRequest) (*v1test.PingResponse, error)
	Ping(context.Context, *rerpc.Request[v1test.PingRequest]) (*rerpc.Response[v1test.PingResponse], error)

	// Can also be implemented in a simplified form:
	// Fail(context.Context, *v1test.FailRequest) (*v1test.FailResponse, error)
	Fail(context.Context, *rerpc.Request[v1test.FailRequest]) (*rerpc.Response[v1test.FailResponse], error)

	Sum(context.Context, *handlerstream.Client[v1test.SumRequest, v1test.SumResponse]) error
	CountUp(context.Context, *rerpc.Request[v1test.CountUpRequest], *handlerstream.Server[v1test.CountUpResponse]) error
	CumSum(context.Context, *handlerstream.Bidirectional[v1test.CumSumRequest, v1test.CumSumResponse]) error
}

// newUnwrappedPingService wraps the service implementation in a rerpc.Service,
// which can then be passed to rerpc.NewServeMux.
//
// By default, services support the binary protobuf and JSON codecs.
func newUnwrappedPingService(svc PingService, opts ...rerpc.HandlerOption) *rerpc.Service {
	handlers := make([]rerpc.Handler, 0, 5)
	opts = append([]rerpc.HandlerOption{
		rerpc.Codec(protobuf.NameBinary, protobuf.NewBinary()),
		rerpc.Codec(protobuf.NameJSON, protobuf.NewJSON()),
	}, opts...)

	ping, err := rerpc.NewUnaryHandler(
		"rerpc.ping.v1test.PingService/Ping", // procedure name
		"rerpc.ping.v1test.PingService",      // reflection name
		svc.Ping,
		opts...,
	)
	if err != nil {
		return rerpc.NewService(nil, err)
	}
	handlers = append(handlers, *ping)

	fail, err := rerpc.NewUnaryHandler(
		"rerpc.ping.v1test.PingService/Fail", // procedure name
		"rerpc.ping.v1test.PingService",      // reflection name
		svc.Fail,
		opts...,
	)
	if err != nil {
		return rerpc.NewService(nil, err)
	}
	handlers = append(handlers, *fail)

	sum, err := rerpc.NewStreamingHandler(
		rerpc.StreamTypeClient,
		"rerpc.ping.v1test.PingService/Sum", // procedure name
		"rerpc.ping.v1test.PingService",     // reflection name
		func(ctx context.Context, sender rerpc.Sender, receiver rerpc.Receiver) {
			typed := handlerstream.NewClient[v1test.SumRequest, v1test.SumResponse](sender, receiver)
			err := svc.Sum(ctx, typed)
			_ = receiver.Close()
			if err != nil {
				if _, ok := rerpc.AsError(err); !ok {
					if errors.Is(err, context.Canceled) {
						err = rerpc.Wrap(rerpc.CodeCanceled, err)
					}
					if errors.Is(err, context.DeadlineExceeded) {
						err = rerpc.Wrap(rerpc.CodeDeadlineExceeded, err)
					}
				}
			}
			_ = sender.Close(err)
		},
		opts...,
	)
	if err != nil {
		return rerpc.NewService(nil, err)
	}
	handlers = append(handlers, *sum)

	countUp, err := rerpc.NewStreamingHandler(
		rerpc.StreamTypeServer,
		"rerpc.ping.v1test.PingService/CountUp", // procedure name
		"rerpc.ping.v1test.PingService",         // reflection name
		func(ctx context.Context, sender rerpc.Sender, receiver rerpc.Receiver) {
			typed := handlerstream.NewServer[v1test.CountUpResponse](sender)
			req, err := rerpc.ReceiveRequest[v1test.CountUpRequest](receiver)
			if err != nil {
				_ = receiver.Close()
				_ = sender.Close(err)
				return
			}
			if err = receiver.Close(); err != nil {
				_ = sender.Close(err)
				return
			}
			err = svc.CountUp(ctx, req, typed)
			if err != nil {
				if _, ok := rerpc.AsError(err); !ok {
					if errors.Is(err, context.Canceled) {
						err = rerpc.Wrap(rerpc.CodeCanceled, err)
					}
					if errors.Is(err, context.DeadlineExceeded) {
						err = rerpc.Wrap(rerpc.CodeDeadlineExceeded, err)
					}
				}
			}
			_ = sender.Close(err)
		},
		opts...,
	)
	if err != nil {
		return rerpc.NewService(nil, err)
	}
	handlers = append(handlers, *countUp)

	cumSum, err := rerpc.NewStreamingHandler(
		rerpc.StreamTypeBidirectional,
		"rerpc.ping.v1test.PingService/CumSum", // procedure name
		"rerpc.ping.v1test.PingService",        // reflection name
		func(ctx context.Context, sender rerpc.Sender, receiver rerpc.Receiver) {
			typed := handlerstream.NewBidirectional[v1test.CumSumRequest, v1test.CumSumResponse](sender, receiver)
			err := svc.CumSum(ctx, typed)
			_ = receiver.Close()
			if err != nil {
				if _, ok := rerpc.AsError(err); !ok {
					if errors.Is(err, context.Canceled) {
						err = rerpc.Wrap(rerpc.CodeCanceled, err)
					}
					if errors.Is(err, context.DeadlineExceeded) {
						err = rerpc.Wrap(rerpc.CodeDeadlineExceeded, err)
					}
				}
			}
			_ = sender.Close(err)
		},
		opts...,
	)
	if err != nil {
		return rerpc.NewService(nil, err)
	}
	handlers = append(handlers, *cumSum)

	return rerpc.NewService(handlers, nil)
}

type pluggablePingServiceServer struct {
	ping    func(context.Context, *rerpc.Request[v1test.PingRequest]) (*rerpc.Response[v1test.PingResponse], error)
	fail    func(context.Context, *rerpc.Request[v1test.FailRequest]) (*rerpc.Response[v1test.FailResponse], error)
	sum     func(context.Context, *handlerstream.Client[v1test.SumRequest, v1test.SumResponse]) error
	countUp func(context.Context, *rerpc.Request[v1test.CountUpRequest], *handlerstream.Server[v1test.CountUpResponse]) error
	cumSum  func(context.Context, *handlerstream.Bidirectional[v1test.CumSumRequest, v1test.CumSumResponse]) error
}

func (s *pluggablePingServiceServer) Ping(ctx context.Context, req *rerpc.Request[v1test.PingRequest]) (*rerpc.Response[v1test.PingResponse], error) {
	return s.ping(ctx, req)
}

func (s *pluggablePingServiceServer) Fail(ctx context.Context, req *rerpc.Request[v1test.FailRequest]) (*rerpc.Response[v1test.FailResponse], error) {
	return s.fail(ctx, req)
}

func (s *pluggablePingServiceServer) Sum(ctx context.Context, stream *handlerstream.Client[v1test.SumRequest, v1test.SumResponse]) error {
	return s.sum(ctx, stream)
}

func (s *pluggablePingServiceServer) CountUp(ctx context.Context, req *rerpc.Request[v1test.CountUpRequest], stream *handlerstream.Server[v1test.CountUpResponse]) error {
	return s.countUp(ctx, req, stream)
}

func (s *pluggablePingServiceServer) CumSum(ctx context.Context, stream *handlerstream.Bidirectional[v1test.CumSumRequest, v1test.CumSumResponse]) error {
	return s.cumSum(ctx, stream)
}

// NewPingService wraps the service implementation in a rerpc.Service, ready for
// use with rerpc.NewServeMux. By default, services support the binary protobuf
// and JSON codecs.
//
// The service implementation may mix and match the signatures of PingService
// and the simplified signatures described in its comments. For each method,
// NewPingService first tries to find a simplified implementation. If a simple
// implementation isn't available, it falls back to the more complex
// implementation. If neither is available, rerpc.NewServeMux will return an
// error.
//
// Taken together, this approach lets implementations embed
// UnimplementedPingService and implement each method using whichever signature
// is most convenient.
func NewPingService(svc any, opts ...rerpc.HandlerOption) *rerpc.Service {
	var impl pluggablePingServiceServer

	// Find an implementation of Ping
	if pinger, ok := svc.(interface {
		Ping(context.Context, *v1test.PingRequest) (*v1test.PingResponse, error)
	}); ok {
		impl.ping = func(ctx context.Context, req *rerpc.Request[v1test.PingRequest]) (*rerpc.Response[v1test.PingResponse], error) {
			res, err := pinger.Ping(ctx, req.Msg)
			if err != nil {
				return nil, err
			}
			return rerpc.NewResponse(res), nil
		}
	} else if pinger, ok := svc.(interface {
		Ping(context.Context, *rerpc.Request[v1test.PingRequest]) (*rerpc.Response[v1test.PingResponse], error)
	}); ok {
		impl.ping = pinger.Ping
	} else {
		return rerpc.NewService(nil, errors.New("no Ping implementation found"))
	}

	// Find an implementation of Fail
	if failer, ok := svc.(interface {
		Fail(context.Context, *v1test.FailRequest) (*v1test.FailResponse, error)
	}); ok {
		impl.fail = func(ctx context.Context, req *rerpc.Request[v1test.FailRequest]) (*rerpc.Response[v1test.FailResponse], error) {
			res, err := failer.Fail(ctx, req.Msg)
			if err != nil {
				return nil, err
			}
			return rerpc.NewResponse(res), nil
		}
	} else if failer, ok := svc.(interface {
		Fail(context.Context, *rerpc.Request[v1test.FailRequest]) (*rerpc.Response[v1test.FailResponse], error)
	}); ok {
		impl.fail = failer.Fail
	} else {
		return rerpc.NewService(nil, errors.New("no Fail implementation found"))
	}

	// Find an implementation of Sum
	if sumer, ok := svc.(interface {
		Sum(context.Context, *handlerstream.Client[v1test.SumRequest, v1test.SumResponse]) error
	}); ok {
		impl.sum = sumer.Sum
	} else {
		return rerpc.NewService(nil, errors.New("no Sum implementation found"))
	}

	// Find an implementation of CountUp
	if countUper, ok := svc.(interface {
		CountUp(context.Context, *v1test.CountUpRequest, *handlerstream.Server[v1test.CountUpResponse]) error
	}); ok {
		impl.countUp = func(ctx context.Context, req *rerpc.Request[v1test.CountUpRequest], stream *handlerstream.Server[v1test.CountUpResponse]) error {
			return countUper.CountUp(ctx, req.Msg, stream)
		}
	} else if countUper, ok := svc.(interface {
		CountUp(context.Context, *rerpc.Request[v1test.CountUpRequest], *handlerstream.Server[v1test.CountUpResponse]) error
	}); ok {
		impl.countUp = countUper.CountUp
	} else {
		return rerpc.NewService(nil, errors.New("no CountUp implementation found"))
	}

	// Find an implementation of CumSum
	if cumSumer, ok := svc.(interface {
		CumSum(context.Context, *handlerstream.Bidirectional[v1test.CumSumRequest, v1test.CumSumResponse]) error
	}); ok {
		impl.cumSum = cumSumer.CumSum
	} else {
		return rerpc.NewService(nil, errors.New("no CumSum implementation found"))
	}

	return newUnwrappedPingService(&impl, opts...)
}

var _ PingService = (*UnimplementedPingService)(nil) // verify interface implementation

// UnimplementedPingService returns CodeUnimplemented from all methods.
type UnimplementedPingService struct{}

func (UnimplementedPingService) Ping(context.Context, *rerpc.Request[v1test.PingRequest]) (*rerpc.Response[v1test.PingResponse], error) {
	return nil, rerpc.Errorf(rerpc.CodeUnimplemented, "rerpc.ping.v1test.PingService.Ping isn't implemented")
}

func (UnimplementedPingService) Fail(context.Context, *rerpc.Request[v1test.FailRequest]) (*rerpc.Response[v1test.FailResponse], error) {
	return nil, rerpc.Errorf(rerpc.CodeUnimplemented, "rerpc.ping.v1test.PingService.Fail isn't implemented")
}

func (UnimplementedPingService) Sum(context.Context, *handlerstream.Client[v1test.SumRequest, v1test.SumResponse]) error {
	return rerpc.Errorf(rerpc.CodeUnimplemented, "rerpc.ping.v1test.PingService.Sum isn't implemented")
}

func (UnimplementedPingService) CountUp(context.Context, *rerpc.Request[v1test.CountUpRequest], *handlerstream.Server[v1test.CountUpResponse]) error {
	return rerpc.Errorf(rerpc.CodeUnimplemented, "rerpc.ping.v1test.PingService.CountUp isn't implemented")
}

func (UnimplementedPingService) CumSum(context.Context, *handlerstream.Bidirectional[v1test.CumSumRequest, v1test.CumSumResponse]) error {
	return rerpc.Errorf(rerpc.CodeUnimplemented, "rerpc.ping.v1test.PingService.CumSum isn't implemented")
}
