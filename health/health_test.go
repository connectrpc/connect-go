package health_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/rerpc/rerpc"
	"github.com/rerpc/rerpc/health"
	"github.com/rerpc/rerpc/internal/assert"
	healthrpc "github.com/rerpc/rerpc/internal/gen/proto/go-rerpc/grpc/health/v1"
	pingrpc "github.com/rerpc/rerpc/internal/gen/proto/go-rerpc/rerpc/ping/v1test"
	healthpb "github.com/rerpc/rerpc/internal/gen/proto/go/grpc/health/v1"
)

func TestHealth(t *testing.T) {
	reg := rerpc.NewRegistrar()
	mux, err := rerpc.NewServeMux(
		rerpc.NewNotFoundHandler(),
		pingrpc.NewFullPingService(pingrpc.UnimplementedPingServiceServer{}, reg),
		health.NewService(health.NewChecker(reg)),
	)
	assert.Nil(t, err, "mux construction error")
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client, err := health.NewClient(server.URL, server.Client())
	assert.Nil(t, err, "client construction error")

	const pingFQN = "rerpc.ping.v1test.PingService"
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
		rerr, ok := rerpc.AsError(err)
		assert.True(t, ok, "convert to rerpc error")
		assert.Equal(t, rerr.Code(), rerpc.CodeNotFound, "error code")
	})
	t.Run("watch", func(t *testing.T) {
		client, err := healthrpc.NewHealthClient(
			server.URL,
			server.Client(),
			rerpc.ReplaceProcedurePrefix("internal.", "grpc."),
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
		rerr, ok := rerpc.AsError(err)
		assert.True(t, ok, "convert to rerpc error")
		assert.Equal(t, rerr.Code(), rerpc.CodeUnimplemented, "error code")
	})
}
