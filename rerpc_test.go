package rerpc_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/rerpc/rerpc"
	"github.com/rerpc/rerpc/internal/assert"
	healthpb "github.com/rerpc/rerpc/internal/health/v1"
	pingpb "github.com/rerpc/rerpc/internal/ping/v1test"
	reflectionpb "github.com/rerpc/rerpc/internal/reflection/v1alpha1"
	"github.com/rerpc/rerpc/internal/twirp"
)

const errMsg = "oh no"

type pingServer struct {
	pingpb.UnimplementedPingServiceReRPC
}

func (p pingServer) Ping(ctx context.Context, req *pingpb.PingRequest) (*pingpb.PingResponse, error) {
	return &pingpb.PingResponse{Number: req.Number, Msg: req.Msg}, nil
}

func (p pingServer) Fail(ctx context.Context, req *pingpb.FailRequest) (*pingpb.FailResponse, error) {
	return nil, rerpc.Errorf(rerpc.Code(req.Code), errMsg)
}

func (p pingServer) Sum(ctx context.Context, stream *pingpb.PingServiceReRPC_SumServer) error {
	var sum int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := stream.Receive()
		if err != nil && errors.Is(err, io.EOF) {
			return stream.SendAndClose(&pingpb.SumResponse{
				Sum: sum,
			})
		} else if err != nil {
			return err
		}
		sum += msg.Number
	}
}

func (p pingServer) CountUp(ctx context.Context, req *pingpb.CountUpRequest, stream *pingpb.PingServiceReRPC_CountUpServer) error {
	if req.Number <= 0 {
		return rerpc.Errorf(rerpc.CodeInvalidArgument, "number must be positive: got %v", req.Number)
	}
	for i := int64(1); i <= req.Number; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := stream.Send(&pingpb.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
	return nil
}

func (p pingServer) CumSum(ctx context.Context, stream *pingpb.PingServiceReRPC_CumSumServer) error {
	var sum int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := stream.Receive()
		if err != nil && errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		sum += msg.Number
		if err := stream.Send(&pingpb.CumSumResponse{Sum: sum}); err != nil {
			return err
		}
	}
}

func TestHandlerTwirp(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(pingpb.NewPingServiceHandlerReRPC(pingServer{}))

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

	t.Run("json", func(t *testing.T) {
		t.Run("zero", func(t *testing.T) {
			r, err := http.NewRequest(
				http.MethodPost,
				fmt.Sprintf("%s/internal.ping.v1test.PingService/Ping", server.URL),
				bytes.NewReader(nil),
			)
			assert.Nil(t, err, "create request")
			r.Header.Set("Content-Type", rerpc.TypeJSON)

			response, err := server.Client().Do(r)
			assert.Nil(t, err, "make request")

			testHeaders(t, response)
			assert.Equal(t, response.StatusCode, http.StatusOK, "HTTP status code")
			assertBodyEquals(t, response.Body, "{}") // zero value
		})
		t.Run("ping", func(t *testing.T) {
			probe := `{"number":"42"}`
			r, err := http.NewRequest(
				http.MethodPost,
				fmt.Sprintf("%s/internal.ping.v1test.PingService/Ping", server.URL),
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
				fmt.Sprintf("%s/internal.ping.v1test.PingService/Ping", server.URL),
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
				fmt.Sprintf("%s/internal.ping.v1test.PingService/Fail", server.URL),
				strings.NewReader(probe),
			)
			assert.Nil(t, err, "create request")
			r.Header.Set("Content-Type", rerpc.TypeJSON)

			response, err := server.Client().Do(r)
			assert.Nil(t, err, "make request")
			testHeaders(t, response)
			assert.Equal(t, response.StatusCode, http.StatusTooManyRequests, "HTTP status code")

			expected := &twirp.Status{
				Code:    "resource_exhausted",
				Message: "oh no",
			}
			got := &twirp.Status{}
			contents, err := io.ReadAll(response.Body)
			assert.Nil(t, err, "read response body")
			assert.Nil(t, json.Unmarshal(contents, got), "unmarshal JSON")
			assert.Equal(t, got, expected, "unmarshaled Twirp status")
		})
	})

	t.Run("protobuf", func(t *testing.T) {
		newProtobufReader := func(t testing.TB, msg proto.Message) io.Reader {
			t.Helper()
			bs, err := proto.Marshal(msg)
			assert.Nil(t, err, "marshal request")
			return bytes.NewReader(bs)
		}
		unmarshalResponse := func(t testing.TB, r io.Reader) *pingpb.PingResponse {
			var res pingpb.PingResponse
			bs, err := io.ReadAll(r)
			assert.Nil(t, err, "read body")
			assert.Nil(t, proto.Unmarshal(bs, &res), "unmarshal body")
			return &res
		}
		t.Run("zero", func(t *testing.T) {
			r, err := http.NewRequest(
				http.MethodPost,
				fmt.Sprintf("%s/internal.ping.v1test.PingService/Ping", server.URL),
				bytes.NewReader(nil),
			)
			assert.Nil(t, err, "create request")
			r.Header.Set("Content-Type", rerpc.TypeProtoTwirp)

			response, err := server.Client().Do(r)
			assert.Nil(t, err, "make request")

			testHeaders(t, response)
			assert.Equal(t, response.StatusCode, http.StatusOK, "HTTP status code")
			assertBodyEquals(t, response.Body, "") // zero value
		})
		t.Run("ping", func(t *testing.T) {
			r, err := http.NewRequest(
				http.MethodPost,
				fmt.Sprintf("%s/internal.ping.v1test.PingService/Ping", server.URL),
				newProtobufReader(t, &pingpb.PingRequest{Number: 42}),
			)
			assert.Nil(t, err, "create request")
			r.Header.Set("Content-Type", rerpc.TypeProtoTwirp)

			response, err := server.Client().Do(r)
			assert.Nil(t, err, "make request")

			testHeaders(t, response)
			assert.Equal(t, response.StatusCode, http.StatusOK, "HTTP status code")
			assert.Equal(t, unmarshalResponse(t, response.Body).Number, int64(42), "response")
		})

		t.Run("gzip", func(t *testing.T) {
			probe := newProtobufReader(t, &pingpb.PingRequest{Number: 42})
			buf := &bytes.Buffer{}
			gzipW := gzip.NewWriter(buf)
			io.Copy(gzipW, probe)
			gzipW.Close()

			r, err := http.NewRequest(
				http.MethodPost,
				fmt.Sprintf("%s/internal.ping.v1test.PingService/Ping", server.URL),
				buf,
			)
			assert.Nil(t, err, "create request")
			r.Header.Set("Content-Type", rerpc.TypeProtoTwirp)
			r.Header.Set("Content-Encoding", "gzip")
			r.Header.Set("Accept-Encoding", "gzip")

			response, err := server.Client().Do(r)
			assert.Nil(t, err, "make request")
			testHeaders(t, response)
			assert.Equal(t, response.StatusCode, http.StatusOK, "HTTP status code")
			assert.Equal(t, response.Header.Get("Content-Encoding"), "gzip", "content-encoding header")

			bodyR, err := gzip.NewReader(response.Body)
			assert.Nil(t, err, "read body as gzip")
			assert.Equal(t, unmarshalResponse(t, bodyR).Number, int64(42), "response")
		})

		t.Run("fail", func(t *testing.T) {
			probe := newProtobufReader(t, &pingpb.FailRequest{Code: int32(rerpc.CodeResourceExhausted)})
			r, err := http.NewRequest(
				http.MethodPost,
				fmt.Sprintf("%s/internal.ping.v1test.PingService/Fail", server.URL),
				probe,
			)
			assert.Nil(t, err, "create request")
			r.Header.Set("Content-Type", rerpc.TypeProtoTwirp)

			response, err := server.Client().Do(r)
			assert.Nil(t, err, "make request")
			testHeaders(t, response)
			assert.Equal(t, response.Header.Get("Content-Type"), rerpc.TypeJSON, "error response content-type")
			assert.Equal(t, response.StatusCode, http.StatusTooManyRequests, "HTTP status code")

			expected := &twirp.Status{
				Code:    "resource_exhausted",
				Message: "oh no",
			}
			got := &twirp.Status{}
			contents, err := io.ReadAll(response.Body)
			assert.Nil(t, err, "read response body")
			assert.Nil(t, json.Unmarshal(contents, got), "unmarshal JSON")
			assert.Equal(t, got, expected, "unmarshaled Twirp status")
		})
	})
}

func TestServerProtoGRPC(t *testing.T) {
	const errMsg = "oh no"
	reg := rerpc.NewRegistrar()
	mux := http.NewServeMux()
	mux.Handle(pingpb.NewPingServiceHandlerReRPC(pingServer{}, reg))
	mux.Handle(rerpc.NewHealthHandler(rerpc.NewChecker(reg), reg))
	mux.Handle(rerpc.NewReflectionHandler(reg))

	testPing := func(t *testing.T, client pingpb.PingServiceClientReRPC) {
		t.Run("ping", func(t *testing.T) {
			num := rand.Int63()
			req := &pingpb.PingRequest{Number: num}
			expect := &pingpb.PingResponse{Number: num}
			res, err := client.Ping(context.Background(), req)
			assert.Nil(t, err, "ping error")
			assert.Equal(t, res, expect, "ping response")
		})
	}
	testErrors := func(t *testing.T, client pingpb.PingServiceClientReRPC) {
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
	testHealth := func(t *testing.T, url string, doer rerpc.Doer, opts ...rerpc.CallOption) {
		t.Run("health", func(t *testing.T) {
			const pingFQN = "internal.ping.v1test.PingService"
			const unknown = "foobar"
			assert.True(t, reg.IsRegistered(pingFQN), "ping service registered")
			assert.False(t, reg.IsRegistered(unknown), "unknown service registered")

			callHealth := func(method string, req *healthpb.HealthCheckRequest, opts ...rerpc.CallOption) (*healthpb.HealthCheckResponse, error) {
				ctx, call := rerpc.NewCall(
					context.Background(),
					doer,
					rerpc.StreamTypeUnary,
					url,
					"grpc.health.v1", "Health", method,
					opts...,
				)
				stream := call(ctx)
				if err := stream.Send(req); err != nil {
					_ = stream.CloseSend(err)
					_ = stream.CloseReceive()
					return nil, err
				}
				if err := stream.CloseSend(nil); err != nil {
					_ = stream.CloseReceive()
					return nil, err
				}
				var res healthpb.HealthCheckResponse
				if err := stream.Receive(&res); err != nil {
					_ = stream.CloseReceive()
					return nil, err
				}
				return &res, stream.CloseReceive()
			}

			t.Run("process", func(t *testing.T) {
				req := &healthpb.HealthCheckRequest{}
				res, err := callHealth("Check", req, opts...)
				assert.Nil(t, err, "rpc error")
				assert.Equal(t, rerpc.HealthStatus(res.Status), rerpc.HealthServing, "status")
			})
			t.Run("known", func(t *testing.T) {
				req := &healthpb.HealthCheckRequest{Service: pingFQN}
				res, err := callHealth("Check", req, opts...)
				assert.Nil(t, err, "rpc error")
				assert.Equal(t, rerpc.HealthStatus(res.Status), rerpc.HealthServing, "status")
			})
			t.Run("unknown", func(t *testing.T) {
				req := &healthpb.HealthCheckRequest{Service: unknown}
				_, err := callHealth("Check", req, opts...)
				assert.NotNil(t, err, "rpc error")
				rerr, ok := rerpc.AsError(err)
				assert.True(t, ok, "convert to rerpc error")
				assert.Equal(t, rerr.Code(), rerpc.CodeNotFound, "error code")
			})
			t.Run("watch", func(t *testing.T) {
				req := &healthpb.HealthCheckRequest{Service: pingFQN}
				_, err := callHealth("Watch", req, opts...)
				assert.NotNil(t, err, "rpc error")
				rerr, ok := rerpc.AsError(err)
				assert.True(t, ok, "convert to rerpc error")
				switch rerr.Code() {
				case rerpc.CodeUnimplemented:
					// Expected if we're using HTTP/2.
				case rerpc.CodeUnknown:
					assert.Equal(t, rerr.Error(), "Unknown: HTTP status 505 HTTP Version Not Supported", "error message for CodeUnknown")
				default:
					t.Fatalf("expected CodeUnknown or CodeUnimplemented, got %v", rerr)
				}
			})
		})
	}
	testReflection := func(t *testing.T, url string, doer rerpc.Doer, opts ...rerpc.CallOption) {
		pingRequestFQN := string((&pingpb.PingRequest{}).ProtoReflect().Descriptor().FullName())
		assert.Equal(t, reg.Services(), []string{
			"grpc.health.v1.Health",
			"grpc.reflection.v1alpha.ServerReflection",
			"internal.ping.v1test.PingService",
		}, "services registered in memory")

		callReflect := func(req *reflectionpb.ServerReflectionRequest, opts ...rerpc.CallOption) (*reflectionpb.ServerReflectionResponse, error) {
			ctx, call := rerpc.NewCall(
				context.Background(),
				doer,
				rerpc.StreamTypeUnary,
				url,
				"grpc.reflection.v1alpha", "ServerReflection", "ServerReflectionInfo",
				opts...,
			)
			stream := call(ctx)
			if err := stream.Send(req); err != nil {
				_ = stream.CloseSend(err)
				_ = stream.CloseReceive()
				return nil, err
			}
			if err := stream.CloseSend(nil); err != nil {
				_ = stream.CloseReceive()
				return nil, err
			}
			var res reflectionpb.ServerReflectionResponse
			if err := stream.Receive(&res); err != nil {
				_ = stream.CloseReceive()
				return nil, err
			}
			return &res, stream.CloseReceive()
		}
		t.Run("list_services", func(t *testing.T) {
			req := &reflectionpb.ServerReflectionRequest{
				Host: "some-host",
				MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{
					ListServices: "ignored per proto documentation",
				},
			}
			res, err := callReflect(req, opts...)
			assert.Nil(t, err, "reflection RPC error")
			expect := &reflectionpb.ServerReflectionResponse{
				ValidHost:       req.Host,
				OriginalRequest: req,
				MessageResponse: &reflectionpb.ServerReflectionResponse_ListServicesResponse{
					ListServicesResponse: &reflectionpb.ListServiceResponse{
						Service: []*reflectionpb.ServiceResponse{
							{Name: "grpc.health.v1.Health"},
							{Name: "grpc.reflection.v1alpha.ServerReflection"},
							{Name: "internal.ping.v1test.PingService"},
						},
					},
				},
			}
			assert.Equal(t, res, expect, "response")
		})
		t.Run("file_by_filename", func(t *testing.T) {
			req := &reflectionpb.ServerReflectionRequest{
				Host: "some-host",
				MessageRequest: &reflectionpb.ServerReflectionRequest_FileByFilename{
					FileByFilename: "internal/ping/v1test/ping.proto",
				},
			}
			res, err := callReflect(req, opts...)
			assert.Nil(t, err, "reflection RPC error")
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
			res, err := callReflect(req, opts...)
			assert.Nil(t, err, "reflection RPC error")
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
			res, err := callReflect(req, opts...)
			assert.Nil(t, err, "reflection RPC error")
			msgerr := res.GetErrorResponse()
			assert.NotNil(t, msgerr, "error in response proto")
			assert.Equal(t, msgerr.ErrorCode, int32(rerpc.CodeNotFound), "error code")
			assert.NotZero(t, msgerr.ErrorMessage, "error message")
		})
		t.Run("all_extension_numbers_of_type", func(t *testing.T) {
			req := &reflectionpb.ServerReflectionRequest{
				Host: "some-host",
				MessageRequest: &reflectionpb.ServerReflectionRequest_AllExtensionNumbersOfType{
					AllExtensionNumbersOfType: pingRequestFQN,
				},
			}
			res, err := callReflect(req, opts...)
			assert.Nil(t, err, "reflection RPC error")
			expect := &reflectionpb.ServerReflectionResponse{
				ValidHost:       req.Host,
				OriginalRequest: req,
				MessageResponse: &reflectionpb.ServerReflectionResponse_AllExtensionNumbersResponse{
					AllExtensionNumbersResponse: &reflectionpb.ExtensionNumberResponse{
						BaseTypeName: pingRequestFQN,
					},
				},
			}
			assert.Equal(t, res, expect, "response")
		})
	}
	testMatrix := func(t *testing.T, server *httptest.Server) {
		t.Run("identity", func(t *testing.T) {
			client := pingpb.NewPingServiceClientReRPC(server.URL, server.Client())
			testPing(t, client)
			testErrors(t, client)
			testHealth(t, server.URL, server.Client())
		})
		t.Run("gzip", func(t *testing.T) {
			client := pingpb.NewPingServiceClientReRPC(
				server.URL,
				server.Client(),
				rerpc.Gzip(true),
			)
			testPing(t, client)
			testErrors(t, client)
			testHealth(t, server.URL, server.Client(), rerpc.Gzip(true))
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

type metadataIntegrationInterceptor struct {
	tb         testing.TB
	key, value string
}

func (i *metadataIntegrationInterceptor) Wrap(next rerpc.Func) rerpc.Func {
	return rerpc.Func(func(ctx context.Context, req proto.Message) (proto.Message, error) {
		callMD, isCall := rerpc.CallMeta(ctx)
		handlerMD, isHandler := rerpc.HandlerMeta(ctx)
		assert.False(
			i.tb,
			isCall == isHandler,
			"must be call xor handler: got handler %v, call %v",
			assert.Fmt(isHandler, isCall),
		)

		if isCall {
			// Headers that interceptors can't modify should have been set already.
			assert.Equal(i.tb, callMD.Request().Get("User-Agent"), rerpc.UserAgent(), "request user agent")
			// Server will verify that it received this header.
			assert.Nil(i.tb, callMD.Request().Set(i.key, i.value), "set custom request header")
		}
		if isHandler {
			// Client should have sent both of these headers.
			assert.Equal(i.tb, handlerMD.Request().Get("User-Agent"), rerpc.UserAgent(), "user agent sent by client")
			assert.Equal(i.tb, handlerMD.Request().Get(i.key), i.value, "custom header %q from client", assert.Fmt(i.key))
		}

		res, err := next(ctx, req)

		if isHandler {
			// Client will verify that it receives this header.
			assert.Nil(i.tb, handlerMD.Response().Set(i.key, i.value), "set custom response header")
		}
		if isCall {
			// Server should have sent this response header.
			assert.Equal(i.tb, callMD.Response().Get(i.key), i.value, "custom header %q from server", assert.Fmt(i.key))
		}

		return res, err
	})
}

func (i *metadataIntegrationInterceptor) WrapStream(next rerpc.StreamFunc) rerpc.StreamFunc {
	return next
}

func TestCallMetadataIntegration(t *testing.T) {
	intercept := rerpc.Intercept(&metadataIntegrationInterceptor{tb: t, key: "Foo-Bar", value: "baz"})
	mux := http.NewServeMux()
	mux.Handle(pingpb.NewPingServiceHandlerReRPC(pingServer{}, intercept))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := pingpb.NewPingServiceClientReRPC(server.URL, server.Client(), intercept)

	res, err := client.Ping(context.Background(), &pingpb.PingRequest{})
	assert.Nil(t, err, "call error")
	assert.Equal(t, res, &pingpb.PingResponse{}, "call response")
}
