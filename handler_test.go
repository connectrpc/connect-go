package rerpc_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/rerpc/rerpc"
	"github.com/rerpc/rerpc/internal/assert"
	pingrpc "github.com/rerpc/rerpc/internal/gen/proto/go-rerpc/rerpc/ping/v1test"
	pingpb "github.com/rerpc/rerpc/internal/gen/proto/go/rerpc/ping/v1test"
)

func TestHandlerReadMaxBytes(t *testing.T) {
	const readMaxBytes = 32
	ping, err := rerpc.NewServeMux(
		rerpc.NewNotFoundHandler(),
		pingrpc.NewPingService(
			&ExamplePingServer{},
			rerpc.ReadMaxBytes(readMaxBytes),
		),
	)
	assert.Nil(t, err, "mux construction error")

	server := httptest.NewServer(ping)
	defer server.Close()
	client, err := pingrpc.NewPingServiceClient(server.URL, server.Client())
	assert.Nil(t, err, "client construction error")

	padding := "padding                      "
	req := &pingpb.PingRequest{Number: 42, Msg: padding}
	// Ensure that the probe is actually too big.
	probeBytes, err := proto.Marshal(req)
	assert.Nil(t, err, "marshal request")
	assert.Equal(t, len(probeBytes), readMaxBytes+1, "probe size")

	_, err = client.Ping(context.Background(), req)

	assert.NotNil(t, err, "ping error")
	assert.Equal(t, rerpc.CodeOf(err), rerpc.CodeInvalidArgument, "error code")
	assert.True(
		t,
		strings.Contains(err.Error(), "larger than configured max"),
		`error msg contains "larger than configured max"`,
	)
}
