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
	"net/http"

	healthv1 "github.com/bufbuild/connect/internal/gen/go/connectext/grpc/health/v1"
)

// Status describes the health of a service.
//
// These correspond to the ServingStatus enum in gRPC's health.proto. Because
// connect doesn't support watching health, SERVICE_UNKNOWN isn't aliased here.
//
// For details, see the protobuf schema:
// https://github.com/grpc/grpc/blob/master/src/proto/grpc/health/v1/health.proto.
type HealthStatus = healthv1.HealthCheckResponse_ServingStatus

const (
	// HealthStatusUnspecified indicates that the service's health state is
	// indeterminate.
	HealthStatusUnspecified HealthStatus = healthv1.HealthCheckResponse_SERVING_STATUS_UNSPECIFIED

	// HealthStatusServing indicates that the service is ready to accept
	// requests.
	HealthStatusServing HealthStatus = healthv1.HealthCheckResponse_SERVING_STATUS_UNSPECIFIED

	// HealthStatusNotServing indicates that the process is healthy but the
	// service is not accepting requests.
	HealthStatusNotServing HealthStatus = healthv1.HealthCheckResponse_SERVING_STATUS_UNSPECIFIED
)

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
func NewHealthHandler(checker func(context.Context, string) (HealthStatus, error)) (string, http.Handler) {
	const serviceName = "/grpc.health.v1.Health/"
	mux := http.NewServeMux()
	check := NewUnaryHandler(
		serviceName+"Check",
		func(ctx context.Context, req *Envelope[healthv1.HealthCheckRequest]) (*Envelope[healthv1.HealthCheckResponse], error) {
			status, err := checker(ctx, req.Msg.Service)
			if err != nil {
				return nil, err
			}
			return NewEnvelope(&healthv1.HealthCheckResponse{Status: status}), nil
		},
		// To avoid runtime panics from protobuf registry conflicts with
		// google.golang.org/grpc/health/grpc_health_v1, our copy of health.proto
		// uses a different package name. We're pretending to be the gRPC
		// package, though, so we need to disable reflection to avoid inconsistent
		// package names in the reflection results.
		&disableRegistrationOption{},
	)
	mux.Handle(serviceName+"Check", check)
	watch := NewServerStreamHandler(
		serviceName+"Watch",
		func(
			_ context.Context,
			_ *Envelope[healthv1.HealthCheckRequest],
			_ *ServerStream[healthv1.HealthCheckResponse],
		) error {
			return NewError(
				CodeUnimplemented,
				errors.New("connect doesn't support watching health state"),
			)
		},
		&disableRegistrationOption{}, // see above
	)
	mux.Handle(serviceName+"Watch", watch)
	return serviceName, mux
}
