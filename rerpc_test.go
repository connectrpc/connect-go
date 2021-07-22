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
	reflectionpb "github.com/akshayjshah/rerpc/internal/reflectionpb/v1alpha"
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
	reg := rerpc.NewRegistrar()
	mux := http.NewServeMux()
	mux.Handle(pingpb.NewPingHandlerReRPC(pingServer{}, reg))
	mux.Handle(rerpc.NewReflectionHandler(reg))

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
	testReflection := func(t *testing.T, url string, doer rerpc.Doer, opts ...rerpc.CallOption) {
		url = url + "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo"
		pingRequestFQN := string((&pingpb.PingRequest{}).ProtoReflect().Descriptor().FullName())
		assert.Equal(t, reg.Services(), []string{
			"grpc.reflection.v1alpha.ServerReflection",
			"rerpc.internal.ping.v0.Ping",
		}, "services registered in memory")
		t.Run("list_services", func(t *testing.T) {
			req := &reflectionpb.ServerReflectionRequest{
				Host: "some-host",
				MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{
					ListServices: "ignored per proto documentation",
				},
			}
			var res reflectionpb.ServerReflectionResponse
			assert.Nil(
				t,
				rerpc.Invoke(context.Background(), url, doer, req, &res, opts...),
				"reflection RPC",
			)
			expect := &reflectionpb.ServerReflectionResponse{
				ValidHost:       req.Host,
				OriginalRequest: req,
				MessageResponse: &reflectionpb.ServerReflectionResponse_ListServicesResponse{
					ListServicesResponse: &reflectionpb.ListServiceResponse{
						Service: []*reflectionpb.ServiceResponse{
							&reflectionpb.ServiceResponse{Name: "grpc.reflection.v1alpha.ServerReflection"},
							&reflectionpb.ServiceResponse{Name: "rerpc.internal.ping.v0.Ping"},
						},
					},
				},
			}
			assert.Equal(t, &res, expect, "response")
		})
		t.Run("file_by_filename", func(t *testing.T) {
			req := &reflectionpb.ServerReflectionRequest{
				Host: "some-host",
				MessageRequest: &reflectionpb.ServerReflectionRequest_FileByFilename{
					FileByFilename: "internal/pingpb/ping.proto",
				},
			}
			var res reflectionpb.ServerReflectionResponse
			assert.Nil(
				t,
				rerpc.Invoke(context.Background(), url, doer, req, &res, opts...),
				"reflection RPC",
			)
			assert.Nil(t, res.GetErrorResponse(), "error in response")
			fds := res.GetFileDescriptorResponse()
			assert.NotNil(t, fds, "file descriptor response")
			assert.Equal(t, len(fds.FileDescriptorProto), 1, "number of fds returned")
			assert.True(
				t,
				bytes.Contains(fds.FileDescriptorProto[0], []byte(pingRequestFQN)),
				"fd should contain PingRequest struct",
			)
		})
		t.Run("file_containing_symbol", func(t *testing.T) {
			req := &reflectionpb.ServerReflectionRequest{
				Host: "some-host",
				MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingSymbol{
					FileContainingSymbol: pingRequestFQN,
				},
			}
			var res reflectionpb.ServerReflectionResponse
			assert.Nil(
				t,
				rerpc.Invoke(context.Background(), url, doer, req, &res, opts...),
				"reflection RPC",
			)
			assert.Nil(t, res.GetErrorResponse(), "error in response")
			fds := res.GetFileDescriptorResponse()
			assert.NotNil(t, fds, "file descriptor response")
			assert.Equal(t, len(fds.FileDescriptorProto), 1, "number of fds returned")
			assert.True(
				t,
				bytes.Contains(fds.FileDescriptorProto[0], []byte(pingRequestFQN)),
				"fd should contain PingRequest struct",
			)
		})
		t.Run("file_containing_extension", func(t *testing.T) {
			req := &reflectionpb.ServerReflectionRequest{
				Host: "some-host",
				MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingExtension{
					FileContainingExtension: &reflectionpb.ExtensionRequest{
						ContainingType:  pingRequestFQN,
						ExtensionNumber: 42,
					},
				},
			}
			var res reflectionpb.ServerReflectionResponse
			assert.Nil(
				t,
				rerpc.Invoke(context.Background(), url, doer, req, &res, opts...),
				"reflection RPC",
			)
			expect := &reflectionpb.ServerReflectionResponse{
				ValidHost:       req.Host,
				OriginalRequest: req,
				MessageResponse: &reflectionpb.ServerReflectionResponse_ErrorResponse{
					ErrorResponse: &reflectionpb.ErrorResponse{
						ErrorCode:    int32(rerpc.CodeNotFound),
						ErrorMessage: "proto: not found",
					},
				},
			}
			assert.Equal(t, &res, expect, "response")
		})
		t.Run("all_extension_numbers_of_type", func(t *testing.T) {
			req := &reflectionpb.ServerReflectionRequest{
				Host: "some-host",
				MessageRequest: &reflectionpb.ServerReflectionRequest_AllExtensionNumbersOfType{
					AllExtensionNumbersOfType: pingRequestFQN,
				},
			}
			var res reflectionpb.ServerReflectionResponse
			assert.Nil(
				t,
				rerpc.Invoke(context.Background(), url, doer, req, &res, opts...),
				"reflection RPC",
			)
			expect := &reflectionpb.ServerReflectionResponse{
				ValidHost:       req.Host,
				OriginalRequest: req,
				MessageResponse: &reflectionpb.ServerReflectionResponse_AllExtensionNumbersResponse{
					AllExtensionNumbersResponse: &reflectionpb.ExtensionNumberResponse{
						BaseTypeName: pingRequestFQN,
					},
				},
			}
			assert.Equal(t, &res, expect, "response")
		})
	}
	testMatrix := func(t *testing.T, server *httptest.Server) {
		t.Run("identity", func(t *testing.T) {
			client := pingpb.NewPingClientReRPC(server.URL, server.Client())
			testPing(t, client)
			testErrors(t, client)
		})
		t.Run("gzip", func(t *testing.T) {
			client := pingpb.NewPingClientReRPC(server.URL, server.Client(), rerpc.GzipRequests(true))
			testPing(t, client)
			testErrors(t, client)
		})
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
		testReflection(t, server.URL, server.Client())
	})
}
