package connect_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bufbuild/connect"
	"github.com/bufbuild/connect/internal/assert"
	pingrpc "github.com/bufbuild/connect/internal/gen/proto/go-connect/connect/ping/v1test"
	pingpb "github.com/bufbuild/connect/internal/gen/proto/go/connect/ping/v1test"
	"google.golang.org/protobuf/proto"
)

func TestHandlerReadMaxBytes(t *testing.T) {
	const readMaxBytes = 32
	mux := http.NewServeMux()
	mux.Handle(pingrpc.NewPingServiceHandler(
		&ExamplePingServer{},
		connect.WithReadMaxBytes(readMaxBytes),
	))

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

	_, err = client.Ping(context.Background(), connect.NewMessage(req))

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
