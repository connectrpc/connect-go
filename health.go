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

package connect

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	healthpb "github.com/bufbuild/connect/internal/gen/proto/go/grpc/health/v1"
)

// Status describes the health of a service.
//
// These correspond to the ServingStatus enum in gRPC's health.proto. Because
// connect doesn't support watching health, SERVICE_UNKNOWN isn't aliased here.
//
// For details, see the protobuf schema:
// https://github.com/grpc/grpc/blob/master/src/proto/grpc/health/v1/health.proto.
type HealthStatus = healthpb.HealthCheckResponse_ServingStatus

const (
	// HealthStatusUnspecified indicates that the service's health state is
	// indeterminate.
	HealthStatusUnspecified HealthStatus = healthpb.HealthCheckResponse_SERVING_STATUS_UNSPECIFIED

	// HealthStatusServing indicates that the service is ready to accept
	// requests.
	HealthStatusServing HealthStatus = healthpb.HealthCheckResponse_SERVING_STATUS_UNSPECIFIED

	// HealthStatusNotServing indicates that the process is healthy but the
	// service is not accepting requests.
	HealthStatusNotServing HealthStatus = healthpb.HealthCheckResponse_SERVING_STATUS_UNSPECIFIED
)

// NewHealthChecker returns a health-checking function that always returns
// HealthStatusServing for the process and all registered services. It's safe
// to call concurrently.
//
// The returned function can be passed to NewHealthHandler.
func NewHealthChecker(reg *Registrar) func(context.Context, string) (HealthStatus, error) {
	return func(_ context.Context, service string) (HealthStatus, error) {
		if service == "" {
			return HealthStatusServing, nil
		}
		if reg.isRegistered(service) {
			return HealthStatusServing, nil
		}
		return HealthStatusUnspecified, NewError(
			CodeNotFound,
			fmt.Errorf("unknown service %s", service),
		)
	}
}

// NewHealthHandler wraps the supplied function to build an HTTP handler for
// gRPC's health-checking API. It returns the path on which to mount the
// handler and the HTTP handler itself.
//
// The supplied health-checking function will be called with a fully-qualified
// protobuf service name (e.g., "acme.ping.v1.PingService"). The function must:
// (1) return StatusUnspecified, StatusServing, or StatusNotServing; (2) return
// the health status of the whole process when called with an empty string; (3)
// return a CodeNotFound error when called with an unknown service; and (4) be
// safe to call concurrently. The function returned by NewChecker satisfies all
// these requirements.
//
// Note that the returned handler only supports the unary Check method, not the
// streaming Watch. As suggested in gRPC's health schema, connect returns
// CodeUnimplemented for the Watch method.
//
// For more details on gRPC's health checking protocol, see
// https://github.com/grpc/grpc/blob/master/doc/health-checking.md and
// https://github.com/grpc/grpc/blob/master/src/proto/grpc/health/v1/health.proto.
func NewHealthHandler(
	checker func(context.Context, string) (HealthStatus, error),
	opts ...HandlerOption,
) (string, http.Handler) {
	const serviceName = "grpc.health.v1.Health"
	mux := http.NewServeMux()
	check := NewUnaryHandler(
		serviceName+"/Check", // procedure name
		serviceName,          // registration name
		func(ctx context.Context, req *Envelope[healthpb.HealthCheckRequest]) (*Envelope[healthpb.HealthCheckResponse], error) {
			status, err := checker(ctx, req.Msg.Service)
			if err != nil {
				return nil, err
			}
			return NewEnvelope(&healthpb.HealthCheckResponse{Status: status}), nil
		},
		opts...,
	)
	mux.Handle("/"+serviceName+"/Check", check)
	watch := NewServerStreamHandler(
		serviceName+"/Watch", // procedure name
		serviceName,          // registration name
		func(
			_ context.Context,
			_ *Envelope[healthpb.HealthCheckRequest],
			_ *ServerStream[healthpb.HealthCheckResponse],
		) error {
			return NewError(
				CodeUnimplemented,
				errors.New("connect doesn't support watching health state"),
			)
		},
		opts...,
	)
	mux.Handle("/"+serviceName+"/Watch", watch)
	return "/" + serviceName + "/", mux
}
