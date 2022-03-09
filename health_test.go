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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bufbuild/connect"
	"github.com/bufbuild/connect/internal/assert"
	pingrpc "github.com/bufbuild/connect/internal/gen/connect/ping/v1"
	healthpb "github.com/bufbuild/connect/internal/gen/go/connectext/grpc/health/v1"
)

func TestHealth(t *testing.T) {
	const (
		pingFQN = "ping.v1.PingService"
		unknown = "foobar"
	)

	reg := connect.NewRegistrar()
	mux := http.NewServeMux()
	mux.Handle(pingrpc.NewPingServiceHandler(
		pingrpc.UnimplementedPingServiceHandler{},
		connect.WithRegistrar(reg),
	))
	mux.Handle(connect.NewHealthHandler(connect.NewHealthChecker(reg)))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client, err := connect.NewClient[healthpb.HealthCheckRequest, healthpb.HealthCheckResponse](
		server.Client(),
		server.URL+"/grpc.health.v1.Health/Check",
		connect.WithGRPC(),
	)
	assert.Nil(t, err)

	t.Run("process", func(t *testing.T) {
		res, err := client.CallUnary(
			context.Background(),
			connect.NewEnvelope(&healthpb.HealthCheckRequest{}),
		)
		assert.Nil(t, err)
		assert.Equal(t, res.Msg.Status, connect.HealthStatusServing)
	})
	t.Run("known", func(t *testing.T) {
		res, err := client.CallUnary(
			context.Background(),
			connect.NewEnvelope(&healthpb.HealthCheckRequest{Service: pingFQN}),
		)
		assert.Nil(t, err)
		assert.Equal(t, res.Msg.Status, connect.HealthStatusServing)
	})
	t.Run("unknown", func(t *testing.T) {
		_, err := client.CallUnary(
			context.Background(),
			connect.NewEnvelope(&healthpb.HealthCheckRequest{Service: unknown}),
		)
		assert.NotNil(t, err)
		var connectErr *connect.Error
		ok := errors.As(err, &connectErr)
		assert.True(t, ok)
		assert.Equal(t, connectErr.Code(), connect.CodeNotFound)
	})
	t.Run("watch", func(t *testing.T) {
		client, err := connect.NewClient[healthpb.HealthCheckRequest, healthpb.HealthCheckResponse](
			server.Client(),
			server.URL+"/grpc.health.v1.Health/Watch",
			connect.WithGRPC(),
		)
		assert.Nil(t, err)
		stream, err := client.CallServerStream(
			context.Background(),
			connect.NewEnvelope(&healthpb.HealthCheckRequest{Service: pingFQN}),
		)
		assert.Nil(t, err)
		defer stream.Close()
		assert.False(t, stream.Receive())
		assert.NotNil(t, stream.Err())
		var connectErr *connect.Error
		ok := errors.As(stream.Err(), &connectErr)
		assert.True(t, ok)
		assert.Equal(t, connectErr.Code(), connect.CodeUnimplemented)
	})
}
