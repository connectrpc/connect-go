package rerpc

import (
	"context"
	"net/http"

	"google.golang.org/protobuf/proto"

	healthpb "github.com/rerpc/rerpc/internal/health/v1"
)

// HealthStatus describes the health of a service.
//
// These correspond to the ServingStatus enum in gRPC's health.proto. Because
// reRPC doesn't support watching health, SERVICE_UNKNOWN isn't exposed here.
//
// For details, see the protobuf schema:
// https://github.com/grpc/grpc/blob/master/src/proto/grpc/health/v1/health.proto.
type HealthStatus int32

const (
	HealthUnknown    HealthStatus = 0 // health state indeterminate
	HealthServing    HealthStatus = 1 // ready to accept requests
	HealthNotServing HealthStatus = 2 // process healthy but service not accepting requests
)

// NewChecker returns a health-checking function that always returns
// HealthServing for the process and all registered services. It's safe to call
// concurrently.
//
// The returned function can be passed to NewHealthHandler.
func NewChecker(reg *Registrar) func(context.Context, string) (HealthStatus, error) {
	return func(_ context.Context, service string) (HealthStatus, error) {
		if service == "" {
			return HealthServing, nil
		}
		if reg.IsRegistered(service) {
			return HealthServing, nil
		}
		return HealthUnknown, errorf(CodeNotFound, "unknown service %s", service)
	}
}

// NewHealthHandler wraps the supplied function to build an HTTP handler for
// gRPC's health-checking API. It returns the HTTP handler and the correct path
// on which to mount it. The health-checking function will be called with a
// fully-qualified protobuf service name (e.g., "acme.ping.v1.PingService").
//
// The supplied health-checking function must: (1) return HealthUnknown,
// HealthServing, or HealthNotServing; (2) return the health status of the
// whole process when called with an empty string; (3) return a
// CodeNotFound error when called with an unknown service; and (4) be safe to
// call concurrently. The function returned by NewChecker satisfies all these
// requirements.
//
// Note that the returned handler only supports the unary Check method, not the
// streaming Watch. As suggested in gRPC's health schema, reRPC returns
// CodeUnimplemented for the Watch method. For more details on gRPC's health
// checking protocol, see:
//   https://github.com/grpc/grpc/blob/master/doc/health-checking.md
//   https://github.com/grpc/grpc/blob/master/src/proto/grpc/health/v1/health.proto
func NewHealthHandler(
	checker func(context.Context, string) (HealthStatus, error),
	opts ...HandlerOption,
) (string, *http.ServeMux) {
	mux := http.NewServeMux()
	interceptor := ConfiguredHandlerInterceptor(opts...)

	checkImplementation := Func(func(ctx context.Context, req proto.Message) (proto.Message, error) {
		typed, ok := req.(*healthpb.HealthCheckRequest)
		if !ok {
			return nil, errorf(
				CodeInternal,
				"can't call grpc.health.v1.Health.Check with a %v",
				req.ProtoReflect().Descriptor().FullName(),
			)
		}
		status, err := checker(ctx, typed.Service)
		if err != nil {
			return nil, err
		}
		return &healthpb.HealthCheckResponse{
			Status: healthpb.HealthCheckResponse_ServingStatus(status),
		}, nil
	})

	if interceptor != nil {
		checkImplementation = interceptor.Wrap(checkImplementation)
	}

	watchImplementation := func(ctx context.Context, sf StreamFunc) {
		if interceptor != nil {
			sf = interceptor.WrapStream(sf)
		}
		stream := sf(ctx)
		_ = stream.CloseReceive()
		_ = stream.CloseSend(errorf(
			CodeUnimplemented,
			"reRPC doesn't support watching health state",
		))
	}

	check := NewHandler(
		StreamTypeUnary,
		"grpc.health.v1", "Health", "Check",
		func(ctx context.Context, sf StreamFunc) {
			stream := sf(ctx)
			defer stream.CloseReceive()
			var req healthpb.HealthCheckRequest
			if err := stream.Receive(&req); err != nil {
				_ = stream.CloseSend(err)
				return
			}
			res, err := checkImplementation(ctx, &req)
			if err != nil {
				_ = stream.CloseSend(err)
				return
			}
			_ = stream.CloseSend(stream.Send(res))
		},
		opts...,
	)
	mux.Handle(check.Path(), check)

	watch := NewHandler(
		StreamTypeBidirectional,
		"grpc.health.v1", "Health", "Watch",
		watchImplementation,
		opts...,
	)
	mux.Handle(watch.Path(), watch)

	mux.Handle("/", NewBadRouteHandler(opts...))
	return watch.ServicePath(), mux
}
