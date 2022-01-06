package rerpc_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/rerpc/rerpc"
	"github.com/rerpc/rerpc/internal/assert"
	pingpb "github.com/rerpc/rerpc/internal/ping/v1test"
)

func TestHandlerReadMaxBytes(t *testing.T) {
	const readMaxBytes = 32
	ping := rerpc.NewServeMux(pingpb.NewPingServiceHandlerReRPC(
		&ExamplePingServer{},
		rerpc.ReadMaxBytes(readMaxBytes),
	))

	server := httptest.NewServer(ping)
	defer server.Close()
	client := pingpb.NewPingServiceClientReRPC(server.URL, server.Client())

	padding := "padding                      "
	req := &pingpb.PingRequest{Number: 42, Msg: padding}
	// Ensure that the probe is actually too big.
	probeBytes, err := proto.Marshal(req)
	assert.Nil(t, err, "marshal request")
	assert.Equal(t, len(probeBytes), readMaxBytes+1, "probe size")

	_, err = client.Ping(context.Background(), rerpc.NewRequest(req))

	assert.NotNil(t, err, "ping error")
	assert.Equal(t, rerpc.CodeOf(err), rerpc.CodeInvalidArgument, "error code")
	assert.True(
		t,
		strings.Contains(err.Error(), "larger than configured max"),
		`error msg contains "larger than configured max"`,
	)
}
