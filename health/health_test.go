package health_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/bufconnect/connect"
	"github.com/bufconnect/connect/health"
	"github.com/bufconnect/connect/internal/assert"
	pingrpc "github.com/bufconnect/connect/internal/gen/proto/go-connect/connect/ping/v1test"
	healthrpc "github.com/bufconnect/connect/internal/gen/proto/go-connect/grpc/health/v1"
	healthpb "github.com/bufconnect/connect/internal/gen/proto/go/grpc/health/v1"
)

func TestHealth(t *testing.T) {
	reg := connect.NewRegistrar()
	mux, err := connect.NewServeMux(
		connect.NewNotFoundHandler(),
		pingrpc.NewPingService(pingrpc.UnimplementedPingService{}, reg),
		health.NewService(health.NewChecker(reg)),
	)
	assert.Nil(t, err, "mux construction error")
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
		rerr, ok := connect.AsError(err)
		assert.True(t, ok, "convert to connect error")
		assert.Equal(t, rerr.Code(), connect.CodeNotFound, "error code")
	})
	t.Run("watch", func(t *testing.T) {
		client, err := healthrpc.NewHealthClient(
			server.URL,
			server.Client(),
			connect.ReplaceProcedurePrefix("internal.", "grpc."),
		)
		assert.Nil(t, err, "client construction error")
		stream, err := client.Watch(
			context.Background(),
			&healthpb.HealthCheckRequest{Service: pingFQN},
		)
		assert.Nil(t, err, "rpc error")
		defer stream.Close()
		_, err = stream.Receive()
		assert.NotNil(t, err, "receive err")
		rerr, ok := connect.AsError(err)
		assert.True(t, ok, "convert to connect error")
		assert.Equal(t, rerr.Code(), connect.CodeUnimplemented, "error code")
	})
}
