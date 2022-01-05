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
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/rerpc/rerpc"
	"github.com/rerpc/rerpc/handlerstream"
	"github.com/rerpc/rerpc/health"
	"github.com/rerpc/rerpc/internal/assert"
	healthpb "github.com/rerpc/rerpc/internal/health/v1"
	pingpb "github.com/rerpc/rerpc/internal/ping/v1test"
	"github.com/rerpc/rerpc/internal/twirp"
	"github.com/rerpc/rerpc/reflection"
)

const errMsg = "oh no"

type pingServer struct {
	pingpb.UnimplementedPingServiceReRPC
}

func (p pingServer) Ping(ctx context.Context, req *rerpc.Request[pingpb.PingRequest]) (*rerpc.Response[pingpb.PingResponse], error) {
	return rerpc.NewResponse(&pingpb.PingResponse{
		Number: req.Msg.Number,
		Msg:    req.Msg.Msg,
	}), nil
}

func (p pingServer) Fail(ctx context.Context, req *rerpc.Request[pingpb.FailRequest]) (*rerpc.Response[pingpb.FailResponse], error) {
	return nil, rerpc.Errorf(rerpc.Code(req.Msg.Code), errMsg)
}

func (p pingServer) Sum(
	ctx context.Context,
	stream *handlerstream.Client[pingpb.SumRequest, pingpb.SumResponse],
) error {
	var sum int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&pingpb.SumResponse{
				Sum: sum,
			})
		} else if err != nil {
			return err
		}
		sum += msg.Number
	}
}

func (p pingServer) CountUp(
	ctx context.Context,
	req *rerpc.Request[pingpb.CountUpRequest],
	stream *handlerstream.Server[pingpb.CountUpResponse],
) error {
	if req.Msg.Number <= 0 {
		return rerpc.Errorf(rerpc.CodeInvalidArgument, "number must be positive: got %v", req.Msg.Number)
	}
	for i := int64(1); i <= req.Msg.Number; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := stream.Send(&pingpb.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
	return nil
}

func (p pingServer) CumSum(
	ctx context.Context,
	stream *handlerstream.Bidirectional[pingpb.CumSumRequest, pingpb.CumSumResponse],
) error {
	var sum int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
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
	mux := rerpc.NewServeMux(
		pingpb.NewPingServiceHandlerReRPC(pingServer{}),
		rerpc.NewBadRouteHandler(),
	)
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

	assertError := func(t testing.TB, response *http.Response, expected *twirp.Status) {
		t.Helper()
		assert.Equal(t, response.Header.Get("Content-Type"), rerpc.TypeJSON, "error response content-type")
		got := &twirp.Status{}
		contents, err := io.ReadAll(response.Body)
		assert.Nil(t, err, "read response body")
		assert.Nil(t, json.Unmarshal(contents, got), "unmarshal JSON")
		assert.Equal(t, got, expected, "unmarshaled Twirp status")
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
			assertError(t, response, expected)
		})

		t.Run("bad_route", func(t *testing.T) {
			r, err := http.NewRequest(
				http.MethodPost,
				fmt.Sprintf("%s/internal.ping.v1test.PingService/Foo", server.URL),
				bytes.NewReader(nil),
			)
			assert.Nil(t, err, "create request")
			r.Header.Set("Content-Type", rerpc.TypeJSON)

			response, err := server.Client().Do(r)
			assert.Nil(t, err, "make request")
			testHeaders(t, response)
			assert.Equal(t, response.StatusCode, http.StatusNotFound, "HTTP status code")

			expected := &twirp.Status{
				Code:    "bad_route",
				Message: "no handler for procedure ",
			}
			assertError(t, response, expected)
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
			assert.Equal(t, response.StatusCode, http.StatusTooManyRequests, "HTTP status code")

			expected := &twirp.Status{
				Code:    "resource_exhausted",
				Message: "oh no",
			}
			assertError(t, response, expected)
		})

		t.Run("bad_route", func(t *testing.T) {
			r, err := http.NewRequest(
				http.MethodPost,
				fmt.Sprintf("%s/internal.ping.v1test.PingService/Foo", server.URL),
				bytes.NewReader(nil),
			)
			assert.Nil(t, err, "create request")
			r.Header.Set("Content-Type", rerpc.TypeProtoTwirp)

			response, err := server.Client().Do(r)
			assert.Nil(t, err, "make request")

			testHeaders(t, response)
			assert.Equal(t, response.StatusCode, http.StatusNotFound, "HTTP status code")

			expected := &twirp.Status{
				Code:    "bad_route",
				Message: "no handler for procedure ",
			}
			assertError(t, response, expected)
		})
	})
}

func TestServerProtoGRPC(t *testing.T) {
	const errMsg = "oh no"
	reg := rerpc.NewRegistrar()
	mux := rerpc.NewServeMux(
		pingpb.NewPingServiceHandlerReRPC(pingServer{}, reg),
		health.NewHandler(health.NewChecker(reg)),
		reflection.NewHandler(reg),
		rerpc.NewBadRouteHandler(),
	)

	testPing := func(t *testing.T, client pingpb.PingServiceClientReRPC) {
		t.Run("ping", func(t *testing.T) {
			num := rand.Int63()
			req := &pingpb.PingRequest{Number: num}
			expect := &pingpb.PingResponse{Number: num}
			res, err := client.Ping(context.Background(), rerpc.NewRequest(req))
			assert.Nil(t, err, "ping error")
			assert.Equal(t, res.Msg, expect, "ping response")
		})
	}
	testSum := func(t *testing.T, client pingpb.PingServiceClientReRPC) {
		t.Run("sum", func(t *testing.T) {
			const upTo = 10
			const expect = 55 // 1+10 + 2+9 + ... + 5+6 = 55
			stream := client.Sum(context.Background())
			for i := int64(1); i <= upTo; i++ {
				err := stream.Send(&pingpb.SumRequest{Number: i})
				assert.Nil(t, err, "Send %v", assert.Fmt(i))
			}
			res, err := stream.CloseAndReceive()
			assert.Nil(t, err, "CloseAndReceive error")
			assert.Equal(t, res, &pingpb.SumResponse{Sum: expect}, "response")
		})
	}
	testCountUp := func(t *testing.T, client pingpb.PingServiceClientReRPC) {
		t.Run("count_up", func(t *testing.T) {
			const n = 5
			got := make([]int64, 0, n)
			expect := make([]int64, 0, n)
			for i := 1; i <= n; i++ {
				expect = append(expect, int64(i))
			}
			stream, err := client.CountUp(
				context.Background(),
				rerpc.NewRequest(&pingpb.CountUpRequest{Number: n}),
			)
			assert.Nil(t, err, "send error")
			for {
				msg, err := stream.Receive()
				if errors.Is(err, io.EOF) {
					break
				}
				assert.Nil(t, err, "receive error")
				got = append(got, msg.Number)
			}
			err = stream.Close()
			assert.Nil(t, err, "close error")
			assert.Equal(t, got, expect, "responses")
		})
	}
	testCumSum := func(t *testing.T, client pingpb.PingServiceClientReRPC, expectSuccess bool) {
		t.Run("cumsum", func(t *testing.T) {
			send := []int64{3, 5, 1}
			expect := []int64{3, 8, 9}
			var got []int64
			stream := client.CumSum(context.Background())
			if !expectSuccess {
				err := stream.Send(&pingpb.CumSumRequest{})
				assert.Nil(t, err, "first send on HTTP/1.1") // succeeds, haven't gotten response back yet
				assert.Nil(t, stream.CloseSend(), "close send error on HTTP/1.1")
				_, err = stream.Receive()
				assert.NotNil(t, err, "first receive on HTTP/1.1") // should be 505
				assert.True(t, strings.Contains(err.Error(), "HTTP status 505"), "expected 505, got %v", assert.Fmt(err))
				assert.Nil(t, stream.CloseReceive(), "close receive error on HTTP/1.1")
				return
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for i, n := range send {
					err := stream.Send(&pingpb.CumSumRequest{Number: n})
					assert.Nil(t, err, "send error #%v", assert.Fmt(i))
				}
				assert.Nil(t, stream.CloseSend(), "close send error")
			}()
			go func() {
				defer wg.Done()
				for {
					msg, err := stream.Receive()
					if errors.Is(err, io.EOF) {
						break
					}
					assert.Nil(t, err, "receive error")
					got = append(got, msg.Sum)
				}
				assert.Nil(t, stream.CloseReceive(), "close receive error")
			}()
			wg.Wait()
			assert.Equal(t, got, expect, "sums")
		})
	}
	testErrors := func(t *testing.T, client pingpb.PingServiceClientReRPC) {
		t.Run("errors", func(t *testing.T) {
			req := &pingpb.FailRequest{Code: int32(rerpc.CodeResourceExhausted)}
			res, err := client.Fail(context.Background(), rerpc.NewRequest(req))
			assert.Nil(t, res, "fail RPC response")
			assert.NotNil(t, err, "fail RPC error")
			rerr, ok := rerpc.AsError(err)
			assert.True(t, ok, "conversion to *rerpc.Error")
			assert.Equal(t, rerr.Code(), rerpc.CodeResourceExhausted, "error code")
			assert.Equal(t, rerr.Error(), "ResourceExhausted: "+errMsg, "error message")
			assert.Zero(t, rerr.Details(), "error details")
		})
	}
	testBadRoute := func(t *testing.T, client pingpb.PingServiceClientReRPC) {
		t.Run("bad_route", func(t *testing.T) {
			req := &pingpb.PingRequest{}
			res, err := client.Ping(context.Background(), rerpc.NewRequest(req))
			assert.Nil(t, res, "fail RPC response")
			assert.NotNil(t, err, "fail RPC error")
			rerr, ok := rerpc.AsError(err)
			assert.True(t, ok, "conversion to *rerpc.Error")
			assert.Equal(t, rerr.Code(), rerpc.CodeNotFound, "error code")
			assert.Equal(t, rerr.Error(), "NotFound: no handler for procedure ", "error message")
			assert.Zero(t, rerr.Details(), "error details")
		})
	}
	testHealth := func(t *testing.T, url string, doer rerpc.Doer, opts ...rerpc.ClientOption) {
		t.Run("health", func(t *testing.T) {
			const pingFQN = "internal.ping.v1test.PingService"
			const unknown = "foobar"
			assert.True(t, reg.IsRegistered(pingFQN), "ping service registered")
			assert.False(t, reg.IsRegistered(unknown), "unknown service registered")
			client := health.NewClient(url, doer, opts...)

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
				options := append(opts, rerpc.OverrideProtobufPackage("grpc.health.v1"))
				client := healthpb.NewHealthClientReRPC(url, doer, options...)
				stream, err := client.Watch(
					context.Background(),
					rerpc.NewRequest(&healthpb.HealthCheckRequest{Service: pingFQN}),
				)
				assert.Nil(t, err, "rpc error")
				defer stream.Close()
				_, err = stream.Receive()
				assert.NotNil(t, err, "receive err")
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
	testMatrix := func(t *testing.T, server *httptest.Server, bidi bool) {
		t.Run("identity", func(t *testing.T) {
			client := pingpb.NewPingServiceClientReRPC(server.URL, server.Client())
			testPing(t, client)
			testSum(t, client)
			testCountUp(t, client)
			testCumSum(t, client, bidi)
			testErrors(t, client)
			testHealth(t, server.URL, server.Client())

			badRouteClient := pingpb.NewPingServiceClientReRPC(
				server.URL,
				server.Client(),
				rerpc.OverrideProtobufPackage("test.badroute"),
			)
			testBadRoute(t, badRouteClient)
		})
		t.Run("gzip", func(t *testing.T) {
			client := pingpb.NewPingServiceClientReRPC(
				server.URL,
				server.Client(),
				rerpc.Gzip(true),
			)
			testPing(t, client)
			testSum(t, client)
			testCountUp(t, client)
			testCumSum(t, client, bidi)
			testErrors(t, client)
			testHealth(t, server.URL, server.Client(), rerpc.Gzip(true))

			badRouteClient := pingpb.NewPingServiceClientReRPC(
				server.URL,
				server.Client(),
				rerpc.OverrideProtobufPackage("test.badroute"),
			)
			testBadRoute(t, badRouteClient)
		})
	}

	t.Run("http1", func(t *testing.T) {
		server := httptest.NewServer(mux)
		defer server.Close()
		testMatrix(t, server, false /* bidi */)
	})
	t.Run("http2", func(t *testing.T) {
		server := httptest.NewUnstartedServer(mux)
		server.EnableHTTP2 = true
		server.StartTLS()
		defer server.Close()
		testMatrix(t, server, true /* bidi */)
	})
}

type pluggablePingServer struct {
	pingpb.UnimplementedPingServiceReRPC

	ping func(context.Context, *rerpc.Request[pingpb.PingRequest]) (*rerpc.Response[pingpb.PingResponse], error)
}

func (p *pluggablePingServer) Ping(ctx context.Context, req *rerpc.Request[pingpb.PingRequest]) (*rerpc.Response[pingpb.PingResponse], error) {
	return p.ping(ctx, req)
}

func TestHeaderBasic(t *testing.T) {
	const key = "Test-Key"
	const cval, hval = "client value", "handler value"

	srv := &pluggablePingServer{
		ping: func(ctx context.Context, req *rerpc.Request[pingpb.PingRequest]) (*rerpc.Response[pingpb.PingResponse], error) {
			assert.Equal(t, req.Header().Get(key), cval, "expected handler to receive headers")
			res := rerpc.NewResponse(&pingpb.PingResponse{})
			res.Header().Set(key, hval)
			return res, nil
		},
	}
	mux := rerpc.NewServeMux(pingpb.NewPingServiceHandlerReRPC(srv))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := pingpb.NewPingServiceClientReRPC(server.URL, server.Client())
	req := rerpc.NewRequest(&pingpb.PingRequest{})
	req.Header().Set(key, cval)
	res, err := client.Ping(context.Background(), req)
	assert.Nil(t, err, "error making request")
	assert.Equal(t, res.Header().Get(key), hval, "expected client to receive headers")
}

type headerIntegrationInterceptor struct {
	tb         testing.TB
	key, value string
}

func (i *headerIntegrationInterceptor) Wrap(next rerpc.Func) rerpc.Func {
	return rerpc.Func(func(ctx context.Context, req rerpc.AnyRequest) (rerpc.AnyResponse, error) {
		spec := req.Spec()
		// Client should set protocol-related headers like user-agent before the
		// interceptor chain takes effect. Servers should receive user-agent from
		// the client.
		assert.NotZero(i.tb, req.Header().Get("User-Agent"), "user-agent is missing")
		if spec.IsClient {
			// Server will verify that it received this header.
			assert.Nil(
				i.tb,
				req.Header().Set(i.key, i.value),
				"set custom request header",
			)
		} else if spec.IsServer {
			// Client should have sent both of these headers.
			assert.Equal(
				i.tb,
				req.Header().Get(i.key), i.value,
				"custom header %q from client", assert.Fmt(i.key),
			)
		}

		res, err := next(ctx, req)

		if spec.IsClient {
			// Server should have sent this response header.
			assert.Equal(
				i.tb,
				res.Header().Get(i.key), i.value,
				"custom header %q from server", assert.Fmt(i.key),
			)
		} else if spec.IsServer {
			// Client will verify that it receives this header.
			assert.Nil(
				i.tb,
				res.Header().Set(i.key, i.value),
				"set custom response header",
			)
		}

		return res, err
	})
}

func (i *headerIntegrationInterceptor) WrapStream(next rerpc.StreamFunc) rerpc.StreamFunc {
	return next
}

func TestHeaderIntegration(t *testing.T) {
	const key, value = "Foo-Bar", "baz"
	intercept := rerpc.Intercept(&headerIntegrationInterceptor{
		tb:    t,
		key:   key,
		value: value,
	})
	mux := rerpc.NewServeMux(
		pingpb.NewPingServiceHandlerReRPC(pingServer{}, intercept),
	)
	server := httptest.NewServer(mux)
	defer server.Close()
	client := pingpb.NewPingServiceClientReRPC(server.URL, server.Client(), intercept)

	req := rerpc.NewRequest(&pingpb.PingRequest{})
	res, err := client.Ping(context.Background(), req)
	assert.Nil(t, err, "call error")
	assert.Equal(t, res.Msg, &pingpb.PingResponse{}, "call response")
	assert.NotZero(t, req.Header().Get("User-Agent"), "user-agent is missing")
	assert.Equal(t, req.Header().Get(key), value, "header set by interceptor is missing")
}
