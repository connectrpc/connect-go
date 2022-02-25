package health_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bufbuild/connect"
	"github.com/bufbuild/connect/health"
	"github.com/bufbuild/connect/internal/assert"
	pingrpc "github.com/bufbuild/connect/internal/gen/proto/go-connect/connect/ping/v1test"
	healthrpc "github.com/bufbuild/connect/internal/gen/proto/go-connect/grpc/health/v1"
	healthpb "github.com/bufbuild/connect/internal/gen/proto/go/grpc/health/v1"
)

func TestHealth(t *testing.T) {
	reg := connect.NewRegistrar()
	mux := http.NewServeMux()
	mux.Handle(pingrpc.NewPingServiceHandler(
		pingrpc.UnimplementedPingServiceHandler{},
		connect.WithRegistrar(reg),
	))
	mux.Handle(health.NewHandler(health.NewChecker(reg)))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client, err := health.NewClient(server.URL, server.Client())
	assert.Nil(t, err, "client construction error")

	const pingFQN = "connect.ping.v1test.PingService"
	const unknown = "foobar"
	assert.True(t, reg.IsRegistered(pingFQN), "ping service registered")
	assert.False(t, reg.IsRegistered(unknown), "unknown service registered")

	t.Run("process", func(t *testing.T) {
		res, err := client.Check(context.Background(), &health.CheckRequest{})
		assert.Nil(t, err, "rpc error")
		assert.Equal(t, res.Status, health.StatusServing, "status")
	})
	t.Run("known", func(t *testing.T) {
		res, err := client.Check(context.Background(), &health.CheckRequest{Service: pingFQN})
		assert.Nil(t, err, "rpc error")
		assert.Equal(t, health.Status(res.Status), health.StatusServing, "status")
	})
	t.Run("unknown", func(t *testing.T) {
		_, err := client.Check(context.Background(), &health.CheckRequest{Service: unknown})
		assert.NotNil(t, err, "rpc error")
		var connectErr *connect.Error
		ok := errors.As(err, &connectErr)
		assert.True(t, ok, "convert to connect error")
		assert.Equal(t, connectErr.Code(), connect.CodeNotFound, "error code")
	})
	t.Run("watch", func(t *testing.T) {
		client, err := healthrpc.NewHealthClient(
			server.URL,
			server.Client(),
			connect.WithReplaceProcedurePrefix("internal.", "grpc."),
		)
		assert.Nil(t, err, "client construction error")
		stream, err := client.Watch(
			context.Background(),
			connect.NewRequest(&healthpb.HealthCheckRequest{Service: pingFQN}),
		)
		assert.Nil(t, err, "rpc error")
		defer stream.Close()
		_, err = stream.Receive()
		assert.NotNil(t, err, "receive err")
		var connectErr *connect.Error
		ok := errors.As(err, &connectErr)
		assert.True(t, ok, "convert to connect error")
		assert.Equal(t, connectErr.Code(), connect.CodeUnimplemented, "error code")
	})
}
