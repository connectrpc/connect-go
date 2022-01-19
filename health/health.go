// Package health offers support for gRPC's health-checking APIs.
package health

import (
	"context"

	"github.com/rerpc/rerpc"
	"github.com/rerpc/rerpc/handlerstream"
	healthrpc "github.com/rerpc/rerpc/internal/gen/proto/go-rerpc/grpc/health/v1"
	healthpb "github.com/rerpc/rerpc/internal/gen/proto/go/grpc/health/v1"
)

// Status describes the health of a service.
//
// These correspond to the ServingStatus enum in gRPC's health.proto. Because
// reRPC doesn't support watching health, SERVICE_UNKNOWN isn't exposed here.
//
// For details, see the protobuf schema:
// https://github.com/grpc/grpc/blob/master/src/proto/grpc/health/v1/health.proto.
type Status int32

const (
	StatusUnknown    Status = 0 // health state indeterminate
	StatusServing    Status = 1 // ready to accept requests
	StatusNotServing Status = 2 // process healthy but service not accepting requests
)

// A Registrar checks whether a fully-qualified protobuf service name (e.g.,
// "acme.ping.v1.PingService") has been registered.
//
// A *rerpc.Registrar satisfies this interface.
type Registrar interface {
	IsRegistered(string) bool
}

// NewChecker returns a health-checking function that always returns
// StatusServing for the process and all registered services. It's safe to call
// concurrently.
//
// The returned function can be passed to NewHandler.
func NewChecker(reg Registrar) func(context.Context, string) (Status, error) {
	return func(_ context.Context, service string) (Status, error) {
		if service == "" {
			return StatusServing, nil
		}
		if reg.IsRegistered(service) {
			return StatusServing, nil
		}
		return StatusUnknown, rerpc.Errorf(rerpc.CodeNotFound, "unknown service %s", service)
	}
}

type server struct {
	healthrpc.UnimplementedHealthServer

	check func(context.Context, string) (Status, error)
}

func (s *server) Check(ctx context.Context, req *rerpc.Request[healthpb.HealthCheckRequest]) (*rerpc.Response[healthpb.HealthCheckResponse], error) {
	status, err := s.check(ctx, req.Msg.Service)
	if err != nil {
		return nil, err
	}
	return rerpc.NewResponse(&healthpb.HealthCheckResponse{
		Status: healthpb.HealthCheckResponse_ServingStatus(status),
	}), nil
}

func (s *server) Watch(
	_ context.Context,
	_ *rerpc.Request[healthpb.HealthCheckRequest],
	_ *handlerstream.Server[healthpb.HealthCheckResponse],
) error {
	return rerpc.Errorf(rerpc.CodeUnimplemented, "reRPC doesn't support watching health state")
}

// NewService wraps the supplied function to build HTTP handlers for gRPC's
// health-checking API. The health-checking function will be called with a
// fully-qualified protobuf service name (e.g., "acme.ping.v1.PingService").
//
// The supplied health-checking function must: (1) return StatusUnknown,
// StatusServing, or StatusNotServing; (2) return the health status of the
// whole process when called with an empty string; (3) return a
// rerpc.CodeNotFound error when called with an unknown service; and (4) be
// safe to call concurrently. The function returned by NewChecker satisfies all
// these requirements.
//
// Note that the returned handlers only support the unary Check method, not the
// streaming Watch. As suggested in gRPC's health schema, reRPC returns
// rerpc.CodeUnimplemented for the Watch method. For more details on gRPC's
// health checking protocol, see
// https://github.com/grpc/grpc/blob/master/doc/health-checking.md and
// https://github.com/grpc/grpc/blob/master/src/proto/grpc/health/v1/health.proto.
func NewService(
	checker func(context.Context, string) (Status, error),
	opts ...rerpc.HandlerOption,
) *rerpc.Service {
	return healthrpc.NewFullHealth(
		&server{check: checker},
		append(opts, rerpc.OverrideProtobufPackage("grpc.health.v1"))...,
	)
}

// A CheckRequest asks for the status of a fully-qualified protobuf service
// name.
type CheckRequest struct {
	Service string // leave empty for process health
}

// A CheckResponse reports the health status of a service.
type CheckResponse struct {
	Status Status
}

// A Client for any gRPC-compatible health service.
type Client struct {
	health *healthrpc.HealthClient
}

// NewClient constructs a Client.
func NewClient(baseURL string, doer rerpc.Doer, opts ...rerpc.ClientOption) (*Client, error) {
	c, err := healthrpc.NewHealthClient(
		baseURL,
		doer,
		append(opts, rerpc.OverrideProtobufPackage("grpc.health.v1"))...,
	)
	if err != nil {
		return nil, err
	}
	return &Client{c}, nil
}

// Check the health of a service.
func (c *Client) Check(ctx context.Context, req *CheckRequest) (*CheckResponse, error) {
	res, err := c.health.Check(ctx, &healthpb.HealthCheckRequest{Service: req.Service})
	if err != nil {
		return nil, err
	}
	return &CheckResponse{Status: Status(res.Status)}, nil
}
