package rerpc_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/rerpc/rerpc"
	"github.com/rerpc/rerpc/internal/assert"
	pingpb "github.com/rerpc/rerpc/internal/ping/v1test"
	statuspb "github.com/rerpc/rerpc/internal/status/v1"
	"github.com/rerpc/rerpc/internal/twirp"
)

func TestHandler(t *testing.T) {
	const readMaxBytes = 32
	ping := rerpc.NewHandler(
		"internal.ping.v1test.PingService.Ping", // fully-qualified protobuf method
		"internal.ping.v1test.PingService",      // fully-qualified protobuf service
		"internal.ping.v1test",                  // fully-qualified protobuf package
		rerpc.Func(func(ctx context.Context, req proto.Message) (proto.Message, error) {
			return &pingpb.PingResponse{}, nil
		}),
		rerpc.ReadMaxBytes(readMaxBytes),
	)

	t.Run("read_max_bytes", func(t *testing.T) {

		t.Run("twirp/json", func(t *testing.T) {
			probe := `{"number":"42", "msg": "padding"}`
			assert.Equal(t, len(probe), readMaxBytes+1, "probe size")
			r, err := http.NewRequest(
				http.MethodPost,
				"rerpc.io/internal.ping.v1test.PingService/Ping",
				strings.NewReader(probe),
			)
			assert.Nil(t, err, "create request")
			r.Header.Set("Content-Type", rerpc.TypeJSON)

			recorder := httptest.NewRecorder()
			ping.Serve(recorder, r, &pingpb.PingRequest{})
			response := recorder.Result()

			assert.Equal(t, response.StatusCode, http.StatusBadRequest, "HTTP status code")
			expected := &twirp.Status{
				Code:    "malformed",
				Message: "can't unmarshal JSON body",
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
				"rerpc.io/internal.ping.v1test.PingService/Ping",
				bytes.NewReader(probeBytes),
			)
			assert.Nil(t, err, "create request")
			r.Header.Set("Content-Type", rerpc.TypeProtoTwirp)

			recorder := httptest.NewRecorder()
			ping.Serve(recorder, r, &pingpb.PingRequest{})
			response := recorder.Result()

			assert.Equal(t, response.StatusCode, http.StatusBadRequest, "HTTP status code")
			expected := &twirp.Status{
				Code:    "malformed",
				Message: "can't unmarshal Twirp protobuf body",
			}
			got := &twirp.Status{}
			contents, err := io.ReadAll(response.Body)
			assert.Nil(t, err, "read response body")
			assert.Nil(t, json.Unmarshal(contents, got), "unmarshal JSON")
			assert.Equal(t, got, expected, "unmarshaled Twirp status")
		})

		t.Run("grpc", func(t *testing.T) {
			padding := "padding                      "
			probeBytes, err := proto.Marshal(&pingpb.PingRequest{Number: 42, Msg: padding})
			assert.Nil(t, err, "marshal request")
			assert.Equal(t, len(probeBytes), readMaxBytes+1, "probe size")
			r, err := http.NewRequest(
				http.MethodPost,
				"rerpc.io/internal.ping.v1test.PingService/Ping",
				bytes.NewReader(probeBytes),
			)
			assert.Nil(t, err, "create request")
			r.Header.Set("Content-Type", rerpc.TypeDefaultGRPC)

			recorder := httptest.NewRecorder()
			ping.Serve(recorder, r, &pingpb.PingRequest{})
			response := recorder.Result()

			assert.Equal(t, response.StatusCode, http.StatusOK, "HTTP status code")
			expected := statuspb.Status{
				Code:    int32(rerpc.CodeInvalidArgument),
				Message: "can't unmarshal protobuf body",
			}
			var got statuspb.Status
			statusBytes, err := base64.RawStdEncoding.DecodeString(response.Header.Get("Grpc-Status-Details-Bin"))
			assert.Nil(t, err, "status base64 decode")
			err = proto.Unmarshal(statusBytes, &got)
			assert.Nil(t, err, "status unmarshal")
			assert.Equal(t, response.Header.Get("Grpc-Message"), expected.Message, "message")
			assert.Equal(t, response.Header.Get("Grpc-Status"), strconv.Itoa(int(expected.Code)), "message")
			assert.Equal(t, &got, &expected, "status")
		})
	})
}
