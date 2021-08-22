package rerpc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/rerpc/rerpc"
	"github.com/rerpc/rerpc/internal/assert"
	pingpb "github.com/rerpc/rerpc/internal/ping/v1test"
	"github.com/rerpc/rerpc/internal/twirp"
)

func TestHandlerReadMaxBytes(t *testing.T) {
	const readMaxBytes = 32
	ping := rerpc.NewServeMux(pingpb.NewPingServiceHandlerReRPC(
		&ExamplePingServer{},
		rerpc.ReadMaxBytes(readMaxBytes),
	))

	t.Run("twirp/json", func(t *testing.T) {
		probe := `{"number":"42", "msg": "padding"}`
		assert.Equal(t, len(probe), readMaxBytes+1, "probe size")
		r, err := http.NewRequest(
			http.MethodPost,
			"/internal.ping.v1test.PingService/Ping",
			strings.NewReader(probe),
		)
		assert.Nil(t, err, "create request")
		r.Header.Set("Content-Type", rerpc.TypeJSON)

		recorder := httptest.NewRecorder()
		ping.ServeHTTP(recorder, r)
		response := recorder.Result()

		assert.Equal(t, response.StatusCode, http.StatusBadRequest, "HTTP status code")
		expected := &twirp.Status{
			Code:    "invalid_argument",
			Message: "request too large: max bytes set to 32",
		}
		got := &twirp.Status{}
		contents, err := io.ReadAll(response.Body)
		assert.Nil(t, err, "read response body")
		assert.Nil(t, json.Unmarshal(contents, got), "unmarshal JSON")
		assert.Equal(t, got, expected, "unmarshaled Twirp status")
	})

	t.Run("twirp/protobuf", func(t *testing.T) {
		padding := "padding                      "
		probeBytes, err := proto.Marshal(&pingpb.PingRequest{Number: 42, Msg: padding})
		assert.Nil(t, err, "marshal request")
		assert.Equal(t, len(probeBytes), readMaxBytes+1, "probe size")
		r, err := http.NewRequest(
			http.MethodPost,
			"/internal.ping.v1test.PingService/Ping",
			bytes.NewReader(probeBytes),
		)
		assert.Nil(t, err, "create request")
		r.Header.Set("Content-Type", rerpc.TypeProtoTwirp)

		recorder := httptest.NewRecorder()
		ping.ServeHTTP(recorder, r)
		response := recorder.Result()

		assert.Equal(t, response.StatusCode, http.StatusBadRequest, "HTTP status code")
		expected := &twirp.Status{
			Code:    "invalid_argument",
			Message: "request too large: max bytes set to 32",
		}
		got := &twirp.Status{}
		contents, err := io.ReadAll(response.Body)
		assert.Nil(t, err, "read response body")
		assert.Nil(t, json.Unmarshal(contents, got), "unmarshal JSON")
		assert.Equal(t, got, expected, "unmarshaled Twirp status")
	})

	t.Run("grpc", func(t *testing.T) {
		server := httptest.NewServer(ping)
		defer server.Close()
		client := pingpb.NewPingServiceClientReRPC(server.URL, server.Client())

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
	})
}
