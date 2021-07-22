package rerpc_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/akshayjshah/rerpc"
	"github.com/akshayjshah/rerpc/internal/assert"
	"github.com/akshayjshah/rerpc/internal/pingpb/v0"
	"github.com/akshayjshah/rerpc/internal/statuspb/v0"
)

const errMsg = "oh no"

type pingServer struct {
	pingpb.UnimplementedPingServerReRPC
}

func (p pingServer) Ping(ctx context.Context, req *pingpb.PingRequest) (*pingpb.PingResponse, error) {
	return &pingpb.PingResponse{Number: req.Number}, nil
}

func (p pingServer) Fail(ctx context.Context, req *pingpb.FailRequest) (*pingpb.FailResponse, error) {
	return nil, rerpc.Errorf(rerpc.Code(req.Code), errMsg)
}

func TestHandlerJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(pingpb.NewPingHandlerReRPC(pingServer{}))

	server := httptest.NewServer(mux)
	defer server.Close()

	testHeaders := func(t testing.TB, response *http.Response) {
		t.Helper()
		assert.Equal(t, response.ProtoMajor, 1, "HTTP protocol major version")
		// Should be using HTTP's standard Content-Encoding and Accept-Encoding
		// instead.
		assert.Zero(t, response.Header.Get("Grpc-Encoding"), "grpc-encoding header")
		assert.Zero(t, response.Header.Get("Grpc-Accept-Encoding"), "grpc-accept-encoding header")
	}

	assertBodyEquals := func(t testing.TB, body io.Reader, expected string) {
		t.Helper()
		contents, err := io.ReadAll(body)
		assert.Nil(t, err, "read response body")
		assert.Equal(t, string(contents), expected, "body contents")
	}

	t.Run("ping", func(t *testing.T) {
		probe := `{"number":"42"}`
		r, err := http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/rerpc.internal.ping.v0.Ping/Ping", server.URL),
			strings.NewReader(probe),
		)
		assert.Nil(t, err, "create request")
		r.Header.Set("Content-Type", rerpc.TypeJSON)

		response, err := server.Client().Do(r)
		assert.Nil(t, err, "make request")

		testHeaders(t, response)
		assert.Equal(t, response.StatusCode, http.StatusOK, "HTTP status code")
		assertBodyEquals(t, response.Body, probe)
	})

	t.Run("gzip", func(t *testing.T) {
		probe := `{"number":"42"}`
		buf := &bytes.Buffer{}
		gzipW := gzip.NewWriter(buf)
		io.WriteString(gzipW, probe)
		gzipW.Close()

		r, err := http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/rerpc.internal.ping.v0.Ping/Ping", server.URL),
			buf,
		)
		assert.Nil(t, err, "create request")
		r.Header.Set("Content-Type", rerpc.TypeJSON)
		r.Header.Set("Content-Encoding", "gzip")
		r.Header.Set("Accept-Encoding", "gzip")

		response, err := server.Client().Do(r)
		assert.Nil(t, err, "make request")
		testHeaders(t, response)
		assert.Equal(t, response.StatusCode, http.StatusOK, "HTTP status code")
		assert.Equal(t, response.Header.Get("Content-Encoding"), "gzip", "content-encoding header")

		bodyR, err := gzip.NewReader(response.Body)
		assert.Nil(t, err, "read body as gzip")
		assertBodyEquals(t, bodyR, probe)
	})

	t.Run("fail", func(t *testing.T) {
		probe := fmt.Sprintf(`{"code":%d}`, rerpc.CodeResourceExhausted)
		r, err := http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/rerpc.internal.ping.v0.Ping/Fail", server.URL),
			strings.NewReader(probe),
		)
		assert.Nil(t, err, "create request")
		r.Header.Set("Content-Type", rerpc.TypeJSON)

		response, err := server.Client().Do(r)
		assert.Nil(t, err, "make request")
		testHeaders(t, response)
		assert.Equal(t, response.StatusCode, http.StatusTooManyRequests, "HTTP status code")

		expected := &statuspb.Status{
			Code:    int32(rerpc.CodeResourceExhausted),
			Message: "oh no",
		}
		got := &statuspb.Status{}
		contents, err := io.ReadAll(response.Body)
		assert.Nil(t, err, "read response body")
		assert.Nil(t, protojson.Unmarshal(contents, got), "unmarshal JSON")
		assert.Equal(t, got, expected, "statuspb.Status")
	})
}

func TestServerProtoGRPC(t *testing.T) {
	const errMsg = "oh no"
	mux := http.NewServeMux()
	mux.Handle(pingpb.NewPingHandlerReRPC(pingServer{}))

	testPing := func(t *testing.T, client pingpb.PingClientReRPC) {
		t.Run("ping", func(t *testing.T) {
			num := rand.Int63()
			req := &pingpb.PingRequest{Number: num}
			expect := &pingpb.PingResponse{Number: num}
			res, err := client.Ping(context.Background(), req)
			assert.Nil(t, err, "ping error")
			assert.Equal(t, res, expect, "ping response")
		})
	}
	testErrors := func(t *testing.T, client pingpb.PingClientReRPC) {
		t.Run("errors", func(t *testing.T) {
			req := &pingpb.FailRequest{Code: int32(rerpc.CodeResourceExhausted)}
			res, err := client.Fail(context.Background(), req)
			assert.Nil(t, res, "fail RPC response")
			assert.NotNil(t, err, "fail RPC error")
			rerr, ok := rerpc.AsError(err)
			assert.True(t, ok, "conversion to *rerpc.Error")
			assert.Equal(t, rerr.Code(), rerpc.CodeResourceExhausted, "error code")
			assert.Equal(t, rerr.Error(), "ResourceExhausted: "+errMsg, "error message")
			assert.Zero(t, rerr.Details(), "error details")
		})
	}
	testMatrix := func(t *testing.T, server *httptest.Server) {
		t.Run("identity", func(t *testing.T) {
			client := pingpb.NewPingClientReRPC(server.URL, server.Client())
			testPing(t, client)
			testErrors(t, client)
		})
		// TODO: ping and fail gzip
	}

	t.Run("http1", func(t *testing.T) {
		server := httptest.NewServer(mux)
		defer server.Close()
		testMatrix(t, server)
	})
	t.Run("http2", func(t *testing.T) {
		server := httptest.NewUnstartedServer(mux)
		server.EnableHTTP2 = true
		server.StartTLS()
		defer server.Close()
		testMatrix(t, server)
	})
}
