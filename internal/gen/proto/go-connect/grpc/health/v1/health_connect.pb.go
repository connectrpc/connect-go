// Code generated by protoc-gen-go-connect. DO NOT EDIT.
// versions:
// - protoc-gen-go-connect v0.0.1
// - protoc              v3.17.3
// source: grpc/health/v1/health.proto

package healthv1

import (
	context "context"
	errors "errors"
	connect "github.com/bufconnect/connect"
	clientstream "github.com/bufconnect/connect/clientstream"
	protobuf "github.com/bufconnect/connect/codec/protobuf"
	handlerstream "github.com/bufconnect/connect/handlerstream"
	v1 "github.com/bufconnect/connect/internal/gen/proto/go/grpc/health/v1"
	strings "strings"
)

// This is a compile-time assertion to ensure that this generated file and the
// connect package are compatible. If you get a compiler error that this
// constant isn't defined, this code was generated with a version of connect
// newer than the one compiled into your binary. You can fix the problem by
// either regenerating this code with an older version of connect or updating
// the connect version compiled into your binary.
const _ = connect.SupportsCodeGenV0 // requires connect v0.0.1 or later

// WrappedHealthClient is a client for the internal.health.v1.Health service.
//
// It's a simplified wrapper around the full-featured API of
// UnwrappedHealthClient.
type WrappedHealthClient interface {
	// If the requested service is unknown, the call will fail with status
	// NOT_FOUND.
	Check(context.Context, *v1.HealthCheckRequest) (*v1.HealthCheckResponse, error)
	// Performs a watch for the serving status of the requested service.
	// The server will immediately send back a message indicating the current
	// serving status.  It will then subsequently send a new message whenever
	// the service's serving status changes.
	//
	// If the requested service is unknown when the call is received, the
	// server will send a message setting the serving status to
	// SERVICE_UNKNOWN but will *not* terminate the call.  If at some
	// future point, the serving status of the service becomes known, the
	// server will send a new message with the service's serving status.
	//
	// If the call terminates with status UNIMPLEMENTED, then clients
	// should assume this method is not supported and should not retry the
	// call.  If the call terminates with any other status (including OK),
	// clients should retry the call with appropriate exponential backoff.
	Watch(context.Context, *v1.HealthCheckRequest) (*clientstream.Server[v1.HealthCheckResponse], error)
}

// UnwrappedHealthClient is a client for the internal.health.v1.Health service.
// It's more complex than WrappedHealthClient, but it gives callers more
// fine-grained control (e.g., sending and receiving headers).
type UnwrappedHealthClient interface {
	// If the requested service is unknown, the call will fail with status
	// NOT_FOUND.
	Check(context.Context, *connect.Request[v1.HealthCheckRequest]) (*connect.Response[v1.HealthCheckResponse], error)
	// Performs a watch for the serving status of the requested service.
	// The server will immediately send back a message indicating the current
	// serving status.  It will then subsequently send a new message whenever
	// the service's serving status changes.
	//
	// If the requested service is unknown when the call is received, the
	// server will send a message setting the serving status to
	// SERVICE_UNKNOWN but will *not* terminate the call.  If at some
	// future point, the serving status of the service becomes known, the
	// server will send a new message with the service's serving status.
	//
	// If the call terminates with status UNIMPLEMENTED, then clients
	// should assume this method is not supported and should not retry the
	// call.  If the call terminates with any other status (including OK),
	// clients should retry the call with appropriate exponential backoff.
	Watch(context.Context, *connect.Request[v1.HealthCheckRequest]) (*clientstream.Server[v1.HealthCheckResponse], error)
}

// HealthClient is a client for the internal.health.v1.Health service.
type HealthClient struct {
	client unwrappedHealthClient
}

var _ WrappedHealthClient = (*HealthClient)(nil)

// NewHealthClient constructs a client for the internal.health.v1.Health
// service. By default, it uses the binary protobuf codec.
//
// The URL supplied here should be the base URL for the gRPC server (e.g.,
// https://api.acme.com or https://acme.com/grpc).
func NewHealthClient(baseURL string, doer connect.Doer, opts ...connect.ClientOption) (*HealthClient, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	opts = append([]connect.ClientOption{
		connect.Codec(protobuf.NameBinary, protobuf.NewBinary()),
	}, opts...)
	checkFunc, err := connect.NewClientFunc[v1.HealthCheckRequest, v1.HealthCheckResponse](
		doer,
		baseURL,
		"internal.health.v1.Health/Check",
		opts...,
	)
	if err != nil {
		return nil, err
	}
	watchFunc, err := connect.NewClientStream(
		doer,
		connect.StreamTypeServer,
		baseURL,
		"internal.health.v1.Health/Watch",
		opts...,
	)
	if err != nil {
		return nil, err
	}
	return &HealthClient{client: unwrappedHealthClient{
		check: checkFunc,
		watch: watchFunc,
	}}, nil
}

// Check calls internal.health.v1.Health.Check.
func (c *HealthClient) Check(ctx context.Context, req *v1.HealthCheckRequest) (*v1.HealthCheckResponse, error) {
	res, err := c.client.Check(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// Watch calls internal.health.v1.Health.Watch.
func (c *HealthClient) Watch(ctx context.Context, req *v1.HealthCheckRequest) (*clientstream.Server[v1.HealthCheckResponse], error) {
	return c.client.Watch(ctx, connect.NewRequest(req))
}

// Unwrap exposes the underlying generic client. Use it if you need finer
// control (e.g., sending and receiving headers).
func (c *HealthClient) Unwrap() UnwrappedHealthClient {
	return &c.client
}

type unwrappedHealthClient struct {
	check func(context.Context, *connect.Request[v1.HealthCheckRequest]) (*connect.Response[v1.HealthCheckResponse], error)
	watch func(context.Context) (connect.Sender, connect.Receiver)
}

var _ UnwrappedHealthClient = (*unwrappedHealthClient)(nil)

// Check calls internal.health.v1.Health.Check.
func (c *unwrappedHealthClient) Check(ctx context.Context, req *connect.Request[v1.HealthCheckRequest]) (*connect.Response[v1.HealthCheckResponse], error) {
	return c.check(ctx, req)
}

// Watch calls internal.health.v1.Health.Watch.
func (c *unwrappedHealthClient) Watch(ctx context.Context, req *connect.Request[v1.HealthCheckRequest]) (*clientstream.Server[v1.HealthCheckResponse], error) {
	sender, receiver := c.watch(ctx)
	if err := sender.Send(req.Msg); err != nil {
		_ = sender.Close(err)
		_ = receiver.Close()
		return nil, err
	}
	if err := sender.Close(nil); err != nil {
		_ = receiver.Close()
		return nil, err
	}
	return clientstream.NewServer[v1.HealthCheckResponse](receiver), nil
}

// Health is an implementation of the internal.health.v1.Health service.
//
// When writing your code, you can always implement the complete Health
// interface. However, if you don't need to work with headers, you can instead
// implement a simpler version of any or all of the unary methods. Where
// available, the simplified signatures are listed in comments.
//
// NewHealth first tries to find the simplified version of each method, then
// falls back to the more complex version. If neither is implemented,
// connect.NewServeMux will return an error.
type Health interface {
	// If the requested service is unknown, the call will fail with status
	// NOT_FOUND.
	//
	// Can also be implemented in a simplified form:
	// Check(context.Context, *v1.HealthCheckRequest) (*v1.HealthCheckResponse, error)
	Check(context.Context, *connect.Request[v1.HealthCheckRequest]) (*connect.Response[v1.HealthCheckResponse], error)
	// Performs a watch for the serving status of the requested service.
	// The server will immediately send back a message indicating the current
	// serving status.  It will then subsequently send a new message whenever
	// the service's serving status changes.
	//
	// If the requested service is unknown when the call is received, the
	// server will send a message setting the serving status to
	// SERVICE_UNKNOWN but will *not* terminate the call.  If at some
	// future point, the serving status of the service becomes known, the
	// server will send a new message with the service's serving status.
	//
	// If the call terminates with status UNIMPLEMENTED, then clients
	// should assume this method is not supported and should not retry the
	// call.  If the call terminates with any other status (including OK),
	// clients should retry the call with appropriate exponential backoff.
	Watch(context.Context, *connect.Request[v1.HealthCheckRequest], *handlerstream.Server[v1.HealthCheckResponse]) error
}

// newUnwrappedHealth wraps the service implementation in a connect.Service,
// which can then be passed to connect.NewServeMux.
//
// By default, services support the gRPC and gRPC-Web protocols with the binary
// protobuf and JSON codecs.
func newUnwrappedHealth(svc Health, opts ...connect.HandlerOption) *connect.Service {
	handlers := make([]connect.Handler, 0, 2)
	opts = append([]connect.HandlerOption{
		connect.Codec(protobuf.NameBinary, protobuf.NewBinary()),
		connect.Codec(protobuf.NameJSON, protobuf.NewJSON()),
	}, opts...)

	check, err := connect.NewUnaryHandler(
		"internal.health.v1.Health/Check", // procedure name
		"internal.health.v1.Health",       // reflection name
		svc.Check,
		opts...,
	)
	if err != nil {
		return connect.NewService(nil, err)
	}
	handlers = append(handlers, *check)

	watch, err := connect.NewStreamingHandler(
		connect.StreamTypeServer,
		"internal.health.v1.Health/Watch", // procedure name
		"internal.health.v1.Health",       // reflection name
		func(ctx context.Context, sender connect.Sender, receiver connect.Receiver) {
			typed := handlerstream.NewServer[v1.HealthCheckResponse](sender)
			req, err := connect.ReceiveRequest[v1.HealthCheckRequest](receiver)
			if err != nil {
				_ = receiver.Close()
				_ = sender.Close(err)
				return
			}
			if err = receiver.Close(); err != nil {
				_ = sender.Close(err)
				return
			}
			err = svc.Watch(ctx, req, typed)
			if err != nil {
				if _, ok := connect.AsError(err); !ok {
					if errors.Is(err, context.Canceled) {
						err = connect.Wrap(connect.CodeCanceled, err)
					}
					if errors.Is(err, context.DeadlineExceeded) {
						err = connect.Wrap(connect.CodeDeadlineExceeded, err)
					}
				}
			}
			_ = sender.Close(err)
		},
		opts...,
	)
	if err != nil {
		return connect.NewService(nil, err)
	}
	handlers = append(handlers, *watch)

	return connect.NewService(handlers, nil)
}

type pluggableHealthServer struct {
	check func(context.Context, *connect.Request[v1.HealthCheckRequest]) (*connect.Response[v1.HealthCheckResponse], error)
	watch func(context.Context, *connect.Request[v1.HealthCheckRequest], *handlerstream.Server[v1.HealthCheckResponse]) error
}

func (s *pluggableHealthServer) Check(ctx context.Context, req *connect.Request[v1.HealthCheckRequest]) (*connect.Response[v1.HealthCheckResponse], error) {
	return s.check(ctx, req)
}

func (s *pluggableHealthServer) Watch(ctx context.Context, req *connect.Request[v1.HealthCheckRequest], stream *handlerstream.Server[v1.HealthCheckResponse]) error {
	return s.watch(ctx, req, stream)
}

// NewHealth wraps the service implementation in a connect.Service, ready for
// use with connect.NewServeMux. By default, services support the gRPC and
// gRPC-Web protocols with the binary protobuf and JSON codecs.
//
// The service implementation may mix and match the signatures of Health and the
// simplified signatures described in its comments. For each method, NewHealth
// first tries to find a simplified implementation. If a simple implementation
// isn't available, it falls back to the more complex implementation. If neither
// is available, connect.NewServeMux will return an error.
//
// Taken together, this approach lets implementations embed UnimplementedHealth
// and implement each method using whichever signature is most convenient.
func NewHealth(svc any, opts ...connect.HandlerOption) *connect.Service {
	var impl pluggableHealthServer

	// Find an implementation of Check
	if checker, ok := svc.(interface {
		Check(context.Context, *v1.HealthCheckRequest) (*v1.HealthCheckResponse, error)
	}); ok {
		impl.check = func(ctx context.Context, req *connect.Request[v1.HealthCheckRequest]) (*connect.Response[v1.HealthCheckResponse], error) {
			res, err := checker.Check(ctx, req.Msg)
			if err != nil {
				return nil, err
			}
			return connect.NewResponse(res), nil
		}
	} else if checker, ok := svc.(interface {
		Check(context.Context, *connect.Request[v1.HealthCheckRequest]) (*connect.Response[v1.HealthCheckResponse], error)
	}); ok {
		impl.check = checker.Check
	} else {
		return connect.NewService(nil, errors.New("no Check implementation found"))
	}

	// Find an implementation of Watch
	if watcher, ok := svc.(interface {
		Watch(context.Context, *v1.HealthCheckRequest, *handlerstream.Server[v1.HealthCheckResponse]) error
	}); ok {
		impl.watch = func(ctx context.Context, req *connect.Request[v1.HealthCheckRequest], stream *handlerstream.Server[v1.HealthCheckResponse]) error {
			return watcher.Watch(ctx, req.Msg, stream)
		}
	} else if watcher, ok := svc.(interface {
		Watch(context.Context, *connect.Request[v1.HealthCheckRequest], *handlerstream.Server[v1.HealthCheckResponse]) error
	}); ok {
		impl.watch = watcher.Watch
	} else {
		return connect.NewService(nil, errors.New("no Watch implementation found"))
	}

	return newUnwrappedHealth(&impl, opts...)
}

var _ Health = (*UnimplementedHealth)(nil) // verify interface implementation

// UnimplementedHealth returns CodeUnimplemented from all methods.
type UnimplementedHealth struct{}

func (UnimplementedHealth) Check(context.Context, *connect.Request[v1.HealthCheckRequest]) (*connect.Response[v1.HealthCheckResponse], error) {
	return nil, connect.Errorf(connect.CodeUnimplemented, "internal.health.v1.Health.Check isn't implemented")
}

func (UnimplementedHealth) Watch(context.Context, *connect.Request[v1.HealthCheckRequest], *handlerstream.Server[v1.HealthCheckResponse]) error {
	return connect.Errorf(connect.CodeUnimplemented, "internal.health.v1.Health.Watch isn't implemented")
}
