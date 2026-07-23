// Copyright 2021-2026 The Connect Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connecthttp_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/memhttp/memhttptest"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestNewClient_InitFailure(t *testing.T) {
	t.Parallel()
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient,
		"http://127.0.0.1:8080",
		// This triggers an error during initialization, so each call will short circuit returning an error.
		connecthttp.WithSendCompression("invalid"))),
	)
	validateExpectedError := func(t *testing.T, err error) {
		t.Helper()
		assert.NotNil(t, err)
		var connectErr *connect.Error
		assert.True(t, errors.As(err, &connectErr))
		assert.Equal(t, connectErr.Message(), `unknown compression "invalid"`)
	}

	t.Run("unary", func(t *testing.T) {
		t.Parallel()
		_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
		validateExpectedError(t, err)
	})
	t.Run("bidi", func(t *testing.T) {
		t.Parallel()
		_, err := client.CumSum(t.Context())
		validateExpectedError(t, err)
	})
	t.Run("client_stream", func(t *testing.T) {
		t.Parallel()
		_, err := client.Sum(t.Context())
		validateExpectedError(t, err)
	})
	t.Run("server_stream", func(t *testing.T) {
		t.Parallel()
		_, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 3})
		validateExpectedError(t, err)
	})
}

func TestNewClient_UnknownSendCodec(t *testing.T) {
	t.Parallel()
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient,
		"http://127.0.0.1:8080",
		// The codec is never registered with WithCodec, so each call fails.
		connecthttp.WithSendCodec("invalid"))),
	)
	_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
	assert.NotNil(t, err)
	var connectErr *connect.Error
	assert.True(t, errors.As(err, &connectErr))
	assert.Equal(t, connectErr.Code(), connect.CodeUnknown)
	assert.Equal(t, connectErr.Message(), `unknown codec "invalid"`)
}

func TestClientPeer(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)

	run := func(t *testing.T, unaryHTTPMethod string, opts ...connecthttp.Option) {
		t.Helper()
		client := pingv1connect.NewPingServiceClient(connect.NewClient(
			connecthttp.NewTransport(server.Client(), server.URL(), opts...),
			assertPeerInterceptor(t),
		))
		t.Run("unary", func(t *testing.T) {
			ctx, _ := connect.NewClientContext(t.Context())
			_, err := client.Ping(ctx, &pingv1.PingRequest{})
			assert.Nil(t, err)
			clientInfo, _ := connecthttp.ClientInfoForContext(ctx)
			assert.Equal(t, unaryHTTPMethod, clientInfo.HTTPMethod())
			text := strings.Repeat(".", 256)
			r, err := client.Ping(t.Context(), &pingv1.PingRequest{Text: text})
			assert.Nil(t, err)
			assert.Equal(t, r.GetText(), text)
		})
		t.Run("client_stream", func(t *testing.T) {
			ctx := context.Background()
			clientStream, err := client.Sum(ctx)
			assert.Nil(t, err)
			t.Cleanup(func() {
				_, closeErr := clientStream.CloseAndReceive()
				assert.Nil(t, closeErr)
			})
			assert.Nil(t, clientStream.Send(&pingv1.SumRequest{}))
		})
		t.Run("server_stream", func(t *testing.T) {
			ctx := context.Background()
			serverStream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 1})
			assert.Nil(t, err)
			t.Cleanup(func() {
				assert.Nil(t, serverStream.Close())
			})
		})
		t.Run("bidi_stream", func(t *testing.T) {
			ctx := context.Background()
			bidiStream, err := client.CumSum(ctx)
			assert.Nil(t, err)
			t.Cleanup(func() {
				assert.Nil(t, bidiStream.CloseSend())
				assert.Nil(t, bidiStream.Close())
			})
			assert.Nil(t, bidiStream.Send(&pingv1.CumSumRequest{}))
		})
	}

	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		run(t, http.MethodPost)
	})
	t.Run("connect+get", func(t *testing.T) {
		t.Parallel()
		run(t, http.MethodGet,
			connecthttp.WithHTTPGet(),
			connecthttp.WithSendCompression("gzip"),
		)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		run(t, http.MethodPost, connecthttp.WithGRPC())
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		run(t, http.MethodPost, connecthttp.WithGRPCWeb())
	})
}

func TestGetNotModified(t *testing.T) {
	t.Parallel()

	const etag = "some-etag"
	// Handlers should automatically set Vary to include request headers that are
	// part of the RPC protocol.
	expectVary := []string{"Accept-Encoding"}

	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, &notModifiedPingServer{etag: etag})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
		server.URL(),
		connecthttp.WithHTTPGet())),
	)
	// unconditional request
	ctx, info := connect.NewClientContext(t.Context())
	_, err := client.Ping(ctx, &pingv1.PingRequest{})
	assert.Nil(t, err)
	etagValue := info.ResponseHeader().Get("Etag")
	assert.Equal(t, etagValue, etag)
	assert.Equal(t, info.ResponseHeader().Values("Vary"), expectVary)
	httpInfo, _ := info.TransportInfo.(*connecthttp.ClientInfo)
	assert.Equal(t, http.MethodGet, httpInfo.HTTPMethod())

	condCtx, condInfo := connect.NewClientContext(t.Context())
	condInfo.RequestHeader().Set("If-None-Match", etag)
	_, err = client.Ping(condCtx, &pingv1.PingRequest{})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
	assert.True(t, connecthttp.IsNotModifiedError(err))
	var connectErr *connect.Error
	assert.True(t, errors.As(err, &connectErr))
	condHTTPInfo, _ := condInfo.TransportInfo.(*connecthttp.ClientInfo)
	assert.Equal(t, http.MethodGet, condHTTPInfo.HTTPMethod())
}

func TestGetNoContentHeaders(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, &pingServer{})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, http.HandlerFunc(func(respWriter http.ResponseWriter, req *http.Request) {
		if len(req.Header.Values("content-type")) > 0 ||
			len(req.Header.Values("content-encoding")) > 0 ||
			len(req.Header.Values("content-length")) > 0 {
			http.Error(respWriter, "GET request should not include content headers", http.StatusBadRequest)
		}
		mux.ServeHTTP(respWriter, req)
	}))
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
		server.URL(),
		connecthttp.WithHTTPGet())),
	)
	ctx, _ := connect.NewClientContext(t.Context())
	_, err := client.Ping(ctx, &pingv1.PingRequest{})
	assert.Nil(t, err)
	clientInfo, _ := connecthttp.ClientInfoForContext(ctx)
	assert.Equal(t, http.MethodGet, clientInfo.HTTPMethod())
}

func TestConnectionDropped(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, protocol := range []string{connect.ProtocolNameConnect, connect.ProtocolNameGRPC, connect.ProtocolNameGRPCWeb} {
		var opts []connecthttp.Option
		switch protocol {
		case connect.ProtocolNameGRPC:
			opts = []connecthttp.Option{connecthttp.WithGRPC()}
		case connect.ProtocolNameGRPCWeb:
			opts = []connecthttp.Option{connecthttp.WithGRPCWeb()}
		}
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()
			httpClient := httpClientFunc(func(_ *http.Request) (*http.Response, error) {
				return nil, io.EOF
			})
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(httpClient,
				"http://1.2.3.4",
				opts...),
			))
			t.Run("unary", func(t *testing.T) {
				t.Parallel()
				_, err := client.Ping(ctx, &pingv1.PingRequest{})
				assert.NotNil(t, err)
				if !assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable) {
					t.Logf("err = %v\n%#v", err, err)
				}
			})
			t.Run("stream", func(t *testing.T) {
				t.Parallel()
				svrStream, err := client.CountUp(ctx, &pingv1.CountUpRequest{})
				if err == nil {
					t.Cleanup(func() {
						assert.Nil(t, svrStream.Close())
					})
					_, err = svrStream.Receive()
				}
				assert.NotNil(t, err)
				if !assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable) {
					t.Logf("err = %v\n%#v", err, err)
				}
			})
		})
	}
}

func TestSpecSchema(t *testing.T) {
	t.Parallel()
	asserter := &assertSchemaInterceptor{t}
	mux := http.NewServeMux()
	srv := connect.NewServer(asserter.ServerInterceptor)
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)

	server := memhttptest.NewServer(t, mux)
	ctx := t.Context()
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
		server.URL()), asserter.ClientInterceptor),
	)
	t.Run("unary", func(t *testing.T) {
		t.Parallel()
		_, err := client.Ping(ctx, &pingv1.PingRequest{})
		assert.Nil(t, err)
		text := strings.Repeat(".", 256)
		r, err := client.Ping(ctx, &pingv1.PingRequest{Text: text})
		assert.Nil(t, err)
		assert.Equal(t, r.GetText(), text)
	})
	t.Run("bidi_stream", func(t *testing.T) {
		t.Parallel()
		bidiStream, err := client.CumSum(ctx)
		assert.Nil(t, err)
		t.Cleanup(func() {
			assert.Nil(t, bidiStream.CloseSend())
			assert.Nil(t, bidiStream.Close())
		})
		assert.Nil(t, bidiStream.Send(&pingv1.CumSumRequest{}))
	})
}

func TestDynamicClient(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)
	client := connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL()))

	t.Run("unary", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.Ping")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)

		req := dynamicpb.NewMessage(methodDesc.Input())
		req.Set(
			methodDesc.Input().Fields().ByName("number"),
			protoreflect.ValueOfInt64(42),
		)
		rsp := dynamicpb.NewMessage(methodDesc.Output())

		err = client.CallUnary(t.Context(), connect.Spec{
			StreamType:       connect.StreamTypeUnary,
			IdempotencyLevel: connect.IdempotencyNoSideEffects,
			Schema:           methodDesc,
			Procedure:        "/connect.ping.v1.PingService/Ping",
		}, req, rsp)
		assert.Nil(t, err)
		got := rsp.Get(methodDesc.Output().Fields().ByName("number")).Int()
		assert.Equal(t, got, 42)
	})
	t.Run("clientStream", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.Sum")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)

		req := dynamicpb.NewMessage(methodDesc.Input())
		req.Set(
			methodDesc.Input().Fields().ByName("number"),
			protoreflect.ValueOfInt64(42),
		)
		rsp := dynamicpb.NewMessage(methodDesc.Output())

		stream, err := client.CallClientStream(t.Context(), connect.Spec{
			StreamType: connect.StreamTypeClient,
			Schema:     methodDesc,
			Procedure:  "/connect.ping.v1.PingService/Sum",
		})
		assert.Nil(t, err)

		assert.Nil(t, stream.Send(req))
		assert.Nil(t, stream.Send(req))
		assert.Nil(t, stream.CloseSend())
		assert.Nil(t, stream.Receive(rsp))

		got := rsp.Get(methodDesc.Output().Fields().ByName("sum")).Int()
		assert.Equal(t, got, 42*2)
	})
	t.Run("serverStream", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.CountUp")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)

		req := dynamicpb.NewMessage(methodDesc.Input())
		req.Set(
			methodDesc.Input().Fields().ByName("number"),
			protoreflect.ValueOfInt64(2),
		)
		rsp := dynamicpb.NewMessage(methodDesc.Output())

		stream, err := client.CallServerStream(t.Context(), connect.Spec{
			StreamType: connect.StreamTypeServer,
			Schema:     methodDesc,
			Procedure:  "/connect.ping.v1.PingService/CountUp",
		}, req)
		if !assert.Nil(t, err) {
			return
		}

		for i := 1; true; i++ {
			if err := stream.Receive(rsp); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				t.Error(err)
				break
			}
			got := rsp.Get(methodDesc.Output().Fields().ByName("number")).Int()
			assert.Equal(t, got, int64(i))
		}
		assert.Nil(t, stream.Close())
	})
	t.Run("bidi", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.CumSum")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)

		stream, err := client.CallClientStream(t.Context(), connect.Spec{
			StreamType: connect.StreamTypeBidi,
			Schema:     methodDesc,
			Procedure:  "/connect.ping.v1.PingService/CumSum",
		})
		if !assert.Nil(t, err) {
			return
		}

		req := dynamicpb.NewMessage(methodDesc.Input())
		req.Set(
			methodDesc.Input().Fields().ByName("number"),
			protoreflect.ValueOfInt64(42),
		)
		rsp := dynamicpb.NewMessage(methodDesc.Output())

		assert.Nil(t, stream.Send(req))
		assert.Nil(t, stream.CloseSend())
		assert.Nil(t, stream.Receive(rsp))
		assert.Nil(t, stream.Close())
		got := rsp.Get(methodDesc.Output().Fields().ByName("sum")).Int()
		assert.Equal(t, got, 42)
	})
}

func TestClientDeadlineHandling(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping slow test")
	}

	// Note that these tests are not able to reproduce issues with the race
	// detector enabled. That's partly why the makefile only runs "slow"
	// tests with the race detector disabled.

	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	svr := httptest.NewUnstartedServer(http.HandlerFunc(func(respWriter http.ResponseWriter, req *http.Request) {
		if req.Context().Err() != nil {
			return
		}
		mux.ServeHTTP(respWriter, req)
	}))
	svr.Config.ErrorLog = log.New(io.Discard, "", 0) //nolint:forbidigo
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	svr.Config.Protocols = p
	svr.Start()
	t.Cleanup(svr.Close)

	clientProtos := new(http.Protocols)
	clientProtos.SetUnencryptedHTTP2(true)
	client := svr.Client()
	transport, ok := client.Transport.(*http.Transport)
	assert.True(t, ok)
	transport.Protocols = clientProtos

	// This case creates a new connection for each RPC to verify that timeouts during dialing
	// won't cause issues. This is historically easier to reproduce, so it uses a smaller
	// duration, no concurrency, and fewer iterations. This is important because if we used
	// a new connection for each RPC in the bigger test scenario below, we'd encounter other
	// issues related to overwhelming the loopback interface and exhausting ephemeral ports.
	t.Run("dial", func(t *testing.T) {
		t.Parallel()
		transport, ok := client.Transport.(*http.Transport)
		if !assert.True(t, ok) {
			t.FailNow()
		}
		testClientDeadlineBruteForceLoop(t,
			5*time.Second, 5, 1,
			func(ctx context.Context) (string, rpcErrors) {
				httpClient := &http.Client{
					Transport: transport.Clone(),
				}
				client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(httpClient, svr.URL)))
				_, err := client.Ping(ctx, &pingv1.PingRequest{Text: "foo"})
				// Close all connections and make sure to give a little time for the OS to
				// release socket resources to prevent resource exhaustion (such as running
				// out of ephemeral ports).
				httpClient.CloseIdleConnections()
				time.Sleep(time.Millisecond / 2)
				return pingv1connect.PingServicePingProcedure, rpcErrors{recvErr: err}
			},
		)
	})

	// This case creates significantly more load than the above one, but uses a normal
	// client so pools and re-uses connections. It also uses all stream types to send
	// messages, to make sure that all stream implementations handle deadlines correctly.
	// The I/O errors related to deadlines are historically harder to reproduce, so it
	// throws a lot more effort into reproducing, particularly a longer duration for
	// which it will run. It also uses larger messages (by packing requests with
	// unrecognized fields) and compression, to make it more likely to encounter the
	// deadline in the middle of read and write operations.
	t.Run("read-write", func(t *testing.T) {
		t.Parallel()

		var extraField []byte
		extraField = protowire.AppendTag(extraField, 999, protowire.BytesType)
		extraData := make([]byte, 16*1024)
		// use good random data so it's not very compressible
		if _, err := rand.Read(extraData); err != nil {
			t.Fatalf("failed to generate extra payload: %v", err)
			return
		}
		extraField = protowire.AppendBytes(extraField, extraData)

		clientConnect := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(client, svr.URL, connecthttp.WithSendCompression("gzip"))))
		clientGRPC := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(client, svr.URL, connecthttp.WithSendCompression("gzip"), connecthttp.WithGRPCWeb())))
		var count atomic.Int32
		testClientDeadlineBruteForceLoop(t,
			20*time.Second, 200, runtime.GOMAXPROCS(0),
			func(ctx context.Context) (string, rpcErrors) {
				var procedure string
				var errs rpcErrors
				rpcNum := count.Add(1)
				var client pingv1connect.PingServiceClient
				if rpcNum&4 == 0 {
					client = clientConnect
				} else {
					client = clientGRPC
				}
				switch rpcNum & 3 {
				case 0:
					procedure = pingv1connect.PingServicePingProcedure
					_, errs.recvErr = client.Ping(ctx, addUnrecognizedBytes(&pingv1.PingRequest{Text: "foo"}, extraField))
				case 1:
					procedure = pingv1connect.PingServiceSumProcedure
					stream, err := client.Sum(ctx)
					if err != nil {
						errs.recvErr = err
						break
					}
					for range 3 {
						errs.sendErr = stream.Send(addUnrecognizedBytes(&pingv1.SumRequest{Number: 1}, extraField))
						if errs.sendErr != nil {
							break
						}
					}
					_, errs.recvErr = stream.CloseAndReceive()
				case 2:
					procedure = pingv1connect.PingServiceCountUpProcedure
					stream, err := client.CountUp(ctx, addUnrecognizedBytes(&pingv1.CountUpRequest{Number: 3}, extraField))
					errs.recvErr = err
					if err == nil {
						for {
							if _, err := stream.Receive(); err != nil {
								if !errors.Is(err, io.EOF) {
									errs.recvErr = err
								}
								break
							}
						}
						errs.closeRecvErr = stream.Close()
					}
				case 3:
					procedure = pingv1connect.PingServiceCumSumProcedure
					stream, err := client.CumSum(ctx)
					if err != nil {
						errs.recvErr = err
						break
					}
					for range 3 {
						errs.sendErr = stream.Send(addUnrecognizedBytes(&pingv1.CumSumRequest{Number: 1}, extraField))
						if _, errs.recvErr = stream.Receive(); errs.recvErr != nil {
							break
						}
					}
					errs.closeSendErr = stream.CloseSend()
					errs.closeRecvErr = stream.Close()
				}
				return procedure, errs
			},
		)
	})
}

func testClientDeadlineBruteForceLoop(
	t *testing.T,
	duration time.Duration,
	iterationsPerDeadline int,
	parallelism int,
	loopBody func(ctx context.Context) (string, rpcErrors),
) {
	t.Helper()
	testContext, testCancel := context.WithTimeout(t.Context(), duration)
	defer testCancel()
	var rpcCount atomic.Int64

	var wg sync.WaitGroup
	for goroutine := range parallelism {
		wg.Go(func() {
			// We try a range of timeouts since the timing issue is sensitive
			// to execution environment (e.g. CPU, memory, and network speeds).
			// So the lower timeout values may be more likely to trigger an issue
			// in faster environments; higher timeouts for slower environments.
			const minTimeout = 10 * time.Microsecond
			const maxTimeout = 2 * time.Millisecond
			for {
				for timeout := minTimeout; timeout <= maxTimeout; timeout += 10 * time.Microsecond {
					for range iterationsPerDeadline {
						if testContext.Err() != nil {
							return
						}
						ctx, cancel := context.WithTimeout(t.Context(), timeout)
						// We are intentionally not inheriting from testContext, which signals when the
						// test loop should stop and return but need not influence the RPC deadline.
						proc, errs := loopBody(ctx) //nolint:contextcheck
						rpcCount.Add(1)
						cancel()
						type errCase struct {
							err      error
							name     string
							allowEOF bool
						}
						errCases := []errCase{
							{
								err:      errs.sendErr,
								name:     "send error",
								allowEOF: true,
							},
							{
								err:  errs.recvErr,
								name: "receive error",
							},
							{
								err:  errs.closeSendErr,
								name: "close-send error",
							},
							{
								err:  errs.closeRecvErr,
								name: "close-receive error",
							},
						}
						for _, errCase := range errCases {
							err := errCase.err
							if err == nil {
								// operation completed before timeout, try again
								continue
							}
							if errCase.allowEOF && errors.Is(err, io.EOF) {
								continue
							}

							if !assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded) {
								var buf bytes.Buffer
								_, _ = fmt.Fprintf(&buf, "actual %v from %s: %v\n%#v", errCase.name, proc, err, err)
								for {
									err = errors.Unwrap(err)
									if err == nil {
										break
									}
									_, _ = fmt.Fprintf(&buf, "\n  caused by: %#v", err)
								}
								t.Log(buf.String())
								testCancel()
							}
						}
					}
				}
				t.Logf("goroutine %d: repeating duration loop", goroutine)
			}
		})
	}
	wg.Wait()
	t.Logf("Issued %d RPCs.", rpcCount.Load())
}

type rpcErrors struct {
	sendErr      error
	recvErr      error
	closeSendErr error
	closeRecvErr error
}

func addUnrecognizedBytes[M protoreflect.ProtoMessage](msg M, data []byte) M {
	msg.ProtoReflect().SetUnknown(data)
	return msg
}

type notModifiedPingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	etag string
}

func (s *notModifiedPingServer) Ping(
	ctx context.Context,
	_ *pingv1.PingRequest,
) (*pingv1.PingResponse, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	ifNoneMatch := info.RequestHeader().Get("If-None-Match")
	serverInfo, _ := connecthttp.ServerInfoForContext(ctx)
	if serverInfo.HTTPMethod() == http.MethodGet && ifNoneMatch == s.etag {
		info.ResponseHeader().Set("Etag", s.etag)
		return nil, connecthttp.NewNotModifiedError()
	}
	info.ResponseHeader().Set("Etag", s.etag)
	return &pingv1.PingResponse{}, nil
}

func assertPeerInterceptor(tb testing.TB) connect.ClientInterceptor {
	tb.Helper()
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			// The transport populates peer info on the CallInfo when it opens
			// the stream inside next, so observe it once next returns.
			stream, err := next(ctx, spec)
			info, _ := connect.CallInfoForClientContext(ctx)
			assert.NotZero(tb, info.PeerAddr)
			assert.NotZero(tb, info.Protocol)
			assert.NotZero(tb, spec)
			return stream, err
		}
	}
}

type assertSchemaInterceptor struct {
	tb testing.TB
}

func (a *assertSchemaInterceptor) ClientInterceptor(next connect.ClientFunc) connect.ClientFunc {
	return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
		a.assertSchema(spec)
		return next(ctx, spec)
	}
}

func (a *assertSchemaInterceptor) ServerInterceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		a.assertSchema(spec)
		return next(ctx, spec, stream)
	}
}

func (a *assertSchemaInterceptor) assertSchema(spec connect.Spec) {
	if !assert.NotNil(a.tb, spec.Schema) {
		return
	}
	methodDesc, ok := spec.Schema.(protoreflect.MethodDescriptor)
	if assert.True(a.tb, ok) {
		procedure := fmt.Sprintf("/%s/%s", methodDesc.Parent().FullName(), methodDesc.Name())
		assert.Equal(a.tb, procedure, spec.Procedure)
	}
}

type httpClientFunc func(*http.Request) (*http.Response, error)

func (fn httpClientFunc) Do(req *http.Request) (*http.Response, error) {
	return fn(req)
}
