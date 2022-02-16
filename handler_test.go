package connect_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bufconnect/connect"
	"github.com/bufconnect/connect/internal/assert"
	pingrpc "github.com/bufconnect/connect/internal/gen/proto/go-connect/connect/ping/v1test"
	pingpb "github.com/bufconnect/connect/internal/gen/proto/go/connect/ping/v1test"
	"google.golang.org/protobuf/proto"
)

func TestHandlerReadMaxBytes(t *testing.T) {
	const readMaxBytes = 32
	mux, err := connect.NewServeMux(
		pingrpc.WithPingServiceHandler(
			&ExamplePingServer{},
			connect.WithReadMaxBytes(readMaxBytes),
		),
	)
	assert.Nil(t, err, "mux construction error")

	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := pingrpc.NewPingServiceClient(server.URL, server.Client())
	assert.Nil(t, err, "client construction error")

	padding := "padding                      "
	req := &pingpb.PingRequest{Number: 42, Text: padding}
	// Ensure that the probe is actually too big.
	probeBytes, err := proto.Marshal(req)
	assert.Nil(t, err, "marshal request")
	assert.Equal(t, len(probeBytes), readMaxBytes+1, "probe size")

	_, err = client.Ping(context.Background(), connect.NewRequest(req))

	assert.NotNil(t, err, "ping error")
	assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument, "error code")
	const expect = "larger than configured max"
	assert.True(
		t,
		strings.Contains(err.Error(), expect),
		"error msg %q contains %q",
		assert.Fmt(err.Error(), expect),
	)
}
