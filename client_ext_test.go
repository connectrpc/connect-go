// Copyright 2021-2024 The Connect Authors
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

package connect_test

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

	"connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/internal/memhttp/memhttptest"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestNewClient_InitFailure(t *testing.T) {
	t.Parallel()
	client := pingv1connect.NewPingServiceClient(
		http.DefaultClient,
		"http://127.0.0.1:8080",
		// This triggers an error during initialization, so each call will short circuit returning an error.
		connect.WithSendCompression("invalid"),
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
		_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{}))
		validateExpectedError(t, err)
	})

	t.Run("bidi", func(t *testing.T) {
		t.Parallel()
		bidiStream := client.CumSum(context.Background())
		err := bidiStream.Send(&pingv1.CumSumRequest{})
		validateExpectedError(t, err)
	})

	t.Run("client_stream", func(t *testing.T) {
		t.Parallel()
		clientStream := client.Sum(context.Background())
		err := clientStream.Send(&pingv1.SumRequest{})
		validateExpectedError(t, err)
	})

	t.Run("server_stream", func(t *testing.T) {
		t.Parallel()
		_, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{Number: 3}))
		validateExpectedError(t, err)
	})
}

func TestClientPeer(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}))
	server := memhttptest.NewServer(t, mux)

	run := func(t *testing.T, unaryHTTPMethod string, opts ...connect.ClientOption) {
		t.Helper()
		client := pingv1connect.NewPingServiceClient(
			server.Client(),
			server.URL(),
			connect.WithClientOptions(opts...),
			connect.WithInterceptors(&assertPeerInterceptor{t}),
		)
		ctx := context.Background()
		t.Run("unary", func(t *testing.T) {
			unaryReq := connect.NewRequest[pingv1.PingRequest](nil)
			_, err := client.Ping(ctx, unaryReq)
			assert.Nil(t, err)
			assert.Equal(t, unaryHTTPMethod, unaryReq.HTTPMethod())
			text := strings.Repeat(".", 256)
			r, err := client.Ping(ctx, connect.NewRequest(&pingv1.PingRequest{Text: text}))
			assert.Nil(t, err)
			assert.Equal(t, r.Msg.GetText(), text)
		})
		t.Run("client_stream", func(t *testing.T) {
			clientStream := client.Sum(ctx)
			t.Cleanup(func() {
				_, closeErr := clientStream.CloseAndReceive()
				assert.Nil(t, closeErr)
			})
			assert.NotZero(t, clientStream.Peer().Addr)
			assert.NotZero(t, clientStream.Peer().Protocol)
			err := clientStream.Send(&pingv1.SumRequest{})
			assert.Nil(t, err)
		})
		t.Run("server_stream", func(t *testing.T) {
			serverStream, err := client.CountUp(ctx, connect.NewRequest(&pingv1.CountUpRequest{}))
			t.Cleanup(func() {
				assert.Nil(t, serverStream.Close())
			})
			assert.Nil(t, err)
		})
		t.Run("bidi_stream", func(t *testing.T) {
			bidiStream := client.CumSum(ctx)
			t.Cleanup(func() {
				assert.Nil(t, bidiStream.CloseRequest())
				assert.Nil(t, bidiStream.CloseResponse())
			})
			assert.NotZero(t, bidiStream.Peer().Addr)
			assert.NotZero(t, bidiStream.Peer().Protocol)
			err := bidiStream.Send(&pingv1.CumSumRequest{})
			assert.Nil(t, err)
		})
	}

	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		run(t, http.MethodPost)
	})
	t.Run("connect+get", func(t *testing.T) {
		t.Parallel()
		run(t, http.MethodGet,
			connect.WithHTTPGet(),
			connect.WithSendGzip(),
		)
	})
	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		run(t, http.MethodPost, connect.WithGRPC())
	})
	t.Run("grpcweb", func(t *testing.T) {
		t.Parallel()
		run(t, http.MethodPost, connect.WithGRPCWeb())
	})
}

func TestGetNotModified(t *testing.T) {
	t.Parallel()

	const etag = "some-etag"
	// Handlers should automatically set Vary to include request headers that are
	// part of the RPC protocol.
	expectVary := []string{"Accept-Encoding"}

	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(&notModifiedPingServer{etag: etag}))
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL(),
		connect.WithHTTPGet(),
	)
	ctx := context.Background()
	// unconditional request
	unaryReq := connect.NewRequest(&pingv1.PingRequest{})
	res, err := client.Ping(ctx, unaryReq)
	assert.Nil(t, err)
	assert.Equal(t, res.Header().Get("Etag"), etag)
	assert.Equal(t, res.Header().Values("Vary"), expectVary)
	assert.Equal(t, http.MethodGet, unaryReq.HTTPMethod())

	unaryReq = connect.NewRequest(&pingv1.PingRequest{})
	unaryReq.Header().Set("If-None-Match", etag)
	_, err = client.Ping(ctx, unaryReq)
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
	assert.True(t, connect.IsNotModifiedError(err))
	var connectErr *connect.Error
	assert.True(t, errors.As(err, &connectErr))
	assert.Equal(t, connectErr.Meta().Get("Etag"), etag)
	assert.Equal(t, connectErr.Meta().Values("Vary"), expectVary)
	assert.Equal(t, http.MethodGet, unaryReq.HTTPMethod())
}

func TestGetNoContentHeaders(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(&pingServer{}))
	server := memhttptest.NewServer(t, http.HandlerFunc(func(respWriter http.ResponseWriter, req *http.Request) {
		if len(req.Header.Values("content-type")) > 0 ||
			len(req.Header.Values("content-encoding")) > 0 ||
			len(req.Header.Values("content-length")) > 0 {
			http.Error(respWriter, "GET request should not include content headers", http.StatusBadRequest)
		}
		mux.ServeHTTP(respWriter, req)
	}))
	client := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL(),
		connect.WithHTTPGet(),
	)
	ctx := context.Background()

	unaryReq := connect.NewRequest(&pingv1.PingRequest{})
	_, err := client.Ping(ctx, unaryReq)
	assert.Nil(t, err)
	assert.Equal(t, http.MethodGet, unaryReq.HTTPMethod())
}

func TestConnectionDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, protocol := range []string{connect.ProtocolConnect, connect.ProtocolGRPC, connect.ProtocolGRPCWeb} {
		var opts []connect.ClientOption
		switch protocol {
		case connect.ProtocolGRPC:
			opts = []connect.ClientOption{connect.WithGRPC()}
		case connect.ProtocolGRPCWeb:
			opts = []connect.ClientOption{connect.WithGRPCWeb()}
		}
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()
			httpClient := httpClientFunc(func(_ *http.Request) (*http.Response, error) {
				return nil, io.EOF
			})
			client := pingv1connect.NewPingServiceClient(
				httpClient,
				"http://1.2.3.4",
				opts...,
			)
			t.Run("unary", func(t *testing.T) {
				t.Parallel()
				req := connect.NewRequest[pingv1.PingRequest](nil)
				_, err := client.Ping(ctx, req)
				assert.NotNil(t, err)
				if !assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable) {
					t.Logf("err = %v\n%#v", err, err)
				}
			})
			t.Run("stream", func(t *testing.T) {
				t.Parallel()
				req := connect.NewRequest[pingv1.CountUpRequest](nil)
				svrStream, err := client.CountUp(ctx, req)
				if err == nil {
					t.Cleanup(func() {
						assert.Nil(t, svrStream.Close())
					})
					if !assert.False(t, svrStream.Receive()) {
						return
					}
					err = svrStream.Err()
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
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(
		pingServer{},
		connect.WithInterceptors(&assertSchemaInterceptor{t}),
	))
	server := memhttptest.NewServer(t, mux)
	ctx := context.Background()
	client := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL(),
		connect.WithInterceptors(&assertSchemaInterceptor{t}),
	)
	t.Run("unary", func(t *testing.T) {
		t.Parallel()
		unaryReq := connect.NewRequest[pingv1.PingRequest](nil)
		_, err := client.Ping(ctx, unaryReq)
		assert.NotNil(t, unaryReq.Spec().Schema)
		assert.Nil(t, err)
		text := strings.Repeat(".", 256)
		r, err := client.Ping(ctx, connect.NewRequest(&pingv1.PingRequest{Text: text}))
		assert.Nil(t, err)
		assert.Equal(t, r.Msg.GetText(), text)
	})
	t.Run("bidi_stream", func(t *testing.T) {
		t.Parallel()
		bidiStream := client.CumSum(ctx)
		t.Cleanup(func() {
			assert.Nil(t, bidiStream.CloseRequest())
			assert.Nil(t, bidiStream.CloseResponse())
		})
		assert.NotZero(t, bidiStream.Spec().Schema)
		err := bidiStream.Send(&pingv1.CumSumRequest{})
		assert.Nil(t, err)
	})
}

func TestDynamicClient(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}))
	server := memhttptest.NewServer(t, mux)
	ctx := context.Background()
	initializer := func(spec connect.Spec, msg any) error {
		dynamic, ok := msg.(*dynamicpb.Message)
		if !ok {
			return nil
		}
		desc, ok := spec.Schema.(protoreflect.MethodDescriptor)
		if !ok {
			return fmt.Errorf("invalid schema type %T for %T message", spec.Schema, dynamic)
		}
		if spec.IsClient {
			*dynamic = *dynamicpb.NewMessage(desc.Output())
		} else {
			*dynamic = *dynamicpb.NewMessage(desc.Input())
		}
		return nil
	}
	t.Run("unary", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.Ping")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)
		client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](
			server.Client(),
			server.URL()+"/connect.ping.v1.PingService/Ping",
			connect.WithSchema(methodDesc),
			connect.WithIdempotency(connect.IdempotencyNoSideEffects),
			connect.WithResponseInitializer(initializer),
		)
		msg := dynamicpb.NewMessage(methodDesc.Input())
		msg.Set(
			methodDesc.Input().Fields().ByName("number"),
			protoreflect.ValueOfInt64(42),
		)
		res, err := client.CallUnary(ctx, connect.NewRequest(msg))
		assert.Nil(t, err)
		got := res.Msg.Get(methodDesc.Output().Fields().ByName("number")).Int()
		assert.Equal(t, got, 42)
	})
	t.Run("clientStream", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.Sum")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)
		client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](
			server.Client(),
			server.URL()+"/connect.ping.v1.PingService/Sum",
			connect.WithSchema(methodDesc),
			connect.WithResponseInitializer(initializer),
		)
		stream := client.CallClientStream(ctx)
		msg := dynamicpb.NewMessage(methodDesc.Input())
		msg.Set(
			methodDesc.Input().Fields().ByName("number"),
			protoreflect.ValueOfInt64(42),
		)
		assert.Nil(t, stream.Send(msg))
		assert.Nil(t, stream.Send(msg))
		rsp, err := stream.CloseAndReceive()
		if !assert.Nil(t, err) {
			return
		}
		got := rsp.Msg.Get(methodDesc.Output().Fields().ByName("sum")).Int()
		assert.Equal(t, got, 42*2)
	})
	t.Run("serverStream", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.CountUp")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)
		client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](
			server.Client(),
			server.URL()+"/connect.ping.v1.PingService/CountUp",
			connect.WithSchema(methodDesc),
			connect.WithResponseInitializer(initializer),
		)
		msg := dynamicpb.NewMessage(methodDesc.Input())
		msg.Set(
			methodDesc.Input().Fields().ByName("number"),
			protoreflect.ValueOfInt64(2),
		)
		req := connect.NewRequest(msg)
		stream, err := client.CallServerStream(ctx, req)
		if !assert.Nil(t, err) {
			return
		}
		for i := 1; stream.Receive(); i++ {
			out := stream.Msg()
			got := out.Get(methodDesc.Output().Fields().ByName("number")).Int()
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
		client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](
			server.Client(),
			server.URL()+"/connect.ping.v1.PingService/CumSum",
			connect.WithSchema(methodDesc),
			connect.WithResponseInitializer(initializer),
		)
		stream := client.CallBidiStream(ctx)
		msg := dynamicpb.NewMessage(methodDesc.Input())
		msg.Set(
			methodDesc.Input().Fields().ByName("number"),
			protoreflect.ValueOfInt64(42),
		)
		assert.Nil(t, stream.Send(msg))
		assert.Nil(t, stream.CloseRequest())
		out, err := stream.Receive()
		if assert.Nil(t, err) {
			return
		}
		got := out.Get(methodDesc.Output().Fields().ByName("number")).Int()
		assert.Equal(t, got, 42)
	})
	t.Run("option", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.Ping")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)
		optionCalled := false
		client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](
			server.Client(),
			server.URL()+"/connect.ping.v1.PingService/Ping",
			connect.WithSchema(methodDesc),
			connect.WithIdempotency(connect.IdempotencyNoSideEffects),
			connect.WithResponseInitializer(
				func(spec connect.Spec, msg any) error {
					assert.NotNil(t, spec)
					assert.NotNil(t, msg)
					dynamic, ok := msg.(*dynamicpb.Message)
					if !assert.True(t, ok) {
						return fmt.Errorf("unexpected message type: %T", msg)
					}
					*dynamic = *dynamicpb.NewMessage(methodDesc.Output())
					optionCalled = true
					return nil
				},
			),
		)
		msg := dynamicpb.NewMessage(methodDesc.Input())
		msg.Set(
			methodDesc.Input().Fields().ByName("number"),
			protoreflect.ValueOfInt64(42),
		)
		res, err := client.CallUnary(ctx, connect.NewRequest(msg))
		assert.Nil(t, err)
		got := res.Msg.Get(methodDesc.Output().Fields().ByName("number")).Int()
		assert.Equal(t, got, 42)
		assert.True(t, optionCalled)
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

	_, handler := pingv1connect.NewPingServiceHandler(pingServer{})
	svr := httptest.NewUnstartedServer(http.HandlerFunc(func(respWriter http.ResponseWriter, req *http.Request) {
		if req.Context().Err() != nil {
			return
		}
		handler.ServeHTTP(respWriter, req)
	}))
	svr.Config.ErrorLog = log.New(io.Discard, "", 0) //nolint:forbidigo
	svr.EnableHTTP2 = true
	svr.StartTLS()
	t.Cleanup(svr.Close)

	// This case creates a new connection for each RPC to verify that timeouts during dialing
	// won't cause issues. This is historically easier to reproduce, so it uses a smaller
	// duration, no concurrency, and fewer iterations. This is important because if we used
	// a new connection for each RPC in the bigger test scenario below, we'd encounter other
	// issues related to overwhelming the loopback interface and exhausting ephemeral ports.
	t.Run("dial", func(t *testing.T) {
		t.Parallel()
		transport, ok := svr.Client().Transport.(*http.Transport)
		if !assert.True(t, ok) {
			t.FailNow()
		}
		testClientDeadlineBruteForceLoop(t,
			5*time.Second, 5, 1,
			func(ctx context.Context) (string, rpcErrors) {
				httpClient := &http.Client{
					Transport: transport.Clone(),
				}
				client := pingv1connect.NewPingServiceClient(httpClient, svr.URL)
				_, err := client.Ping(ctx, connect.NewRequest(&pingv1.PingRequest{Text: "foo"}))
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

		clientConnect := pingv1connect.NewPingServiceClient(svr.Client(), svr.URL, connect.WithSendGzip())
		clientGRPC := pingv1connect.NewPingServiceClient(svr.Client(), svr.URL, connect.WithSendGzip(), connect.WithGRPCWeb())
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
					_, errs.recvErr = client.Ping(ctx, connect.NewRequest(addUnrecognizedBytes(&pingv1.PingRequest{Text: "foo"}, extraField)))
				case 1:
					procedure = pingv1connect.PingServiceSumProcedure
					stream := client.Sum(ctx)
					for range 3 {
						errs.sendErr = stream.Send(addUnrecognizedBytes(&pingv1.SumRequest{Number: 1}, extraField))
						if errs.sendErr != nil {
							break
						}
					}
					_, errs.recvErr = stream.CloseAndReceive()
				case 2:
					procedure = pingv1connect.PingServiceCountUpProcedure
					var stream *connect.ServerStreamForClient[pingv1.CountUpResponse]
					stream, errs.recvErr = client.CountUp(ctx, connect.NewRequest(addUnrecognizedBytes(&pingv1.CountUpRequest{Number: 3}, extraField)))
					if errs.recvErr == nil {
						for stream.Receive() {
						}
						errs.recvErr = stream.Err()
						errs.closeRecvErr = stream.Close()
					}
				case 3:
					procedure = pingv1connect.PingServiceCumSumProcedure
					stream := client.CumSum(ctx)
					for range 3 {
						errs.sendErr = stream.Send(addUnrecognizedBytes(&pingv1.CumSumRequest{Number: 1}, extraField))
						_, errs.recvErr = stream.Receive()
						if errs.recvErr != nil {
							break
						}
					}
					errs.closeSendErr = stream.CloseRequest()
					errs.closeRecvErr = stream.CloseResponse()
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
	testContext, testCancel := context.WithTimeout(context.Background(), duration)
	defer testCancel()
	var rpcCount atomic.Int64

	var wg sync.WaitGroup
	for goroutine := range parallelism {
		wg.Add(1)
		go func() {
			defer wg.Done()
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
						ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
		}()
	}
	wg.Wait()
	t.Logf("Issued %d RPCs.", rpcCount.Load())
}

type notModifiedPingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	etag string
}

func (s *notModifiedPingServer) Ping(
	_ context.Context,
	req *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
	if req.HTTPMethod() == http.MethodGet && req.Header().Get("If-None-Match") == s.etag {
		return nil, connect.NewNotModifiedError(http.Header{"Etag": []string{s.etag}})
	}
	resp := connect.NewResponse(&pingv1.PingResponse{})
	resp.Header().Set("Etag", s.etag)
	return resp, nil
}

type assertPeerInterceptor struct {
	tb testing.TB
}

func (a *assertPeerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		assert.NotZero(a.tb, req.Peer().Addr)
		assert.NotZero(a.tb, req.Peer().Protocol)
		return next(ctx, req)
	}
}

func (a *assertPeerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		assert.NotZero(a.tb, conn.Peer().Addr)
		assert.NotZero(a.tb, conn.Peer().Protocol)
		assert.NotZero(a.tb, conn.Spec())
		return conn
	}
}

func (a *assertPeerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		assert.NotZero(a.tb, conn.Peer().Addr)
		assert.NotZero(a.tb, conn.Peer().Protocol)
		assert.NotZero(a.tb, conn.Spec())
		return next(ctx, conn)
	}
}

type assertSchemaInterceptor struct {
	tb testing.TB
}

func (a *assertSchemaInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if !assert.NotNil(a.tb, req.Spec().Schema) {
			return next(ctx, req)
		}
		methodDesc, ok := req.Spec().Schema.(protoreflect.MethodDescriptor)
		if assert.True(a.tb, ok) {
			procedure := fmt.Sprintf("/%s/%s", methodDesc.Parent().FullName(), methodDesc.Name())
			assert.Equal(a.tb, procedure, req.Spec().Procedure)
		}
		return next(ctx, req)
	}
}

func (a *assertSchemaInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if !assert.NotNil(a.tb, spec.Schema) {
			return conn
		}
		methodDescriptor, ok := spec.Schema.(protoreflect.MethodDescriptor)
		if assert.True(a.tb, ok) {
			procedure := fmt.Sprintf("/%s/%s", methodDescriptor.Parent().FullName(), methodDescriptor.Name())
			assert.Equal(a.tb, procedure, spec.Procedure)
		}
		return conn
	}
}

func (a *assertSchemaInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if !assert.NotNil(a.tb, conn.Spec().Schema) {
			return next(ctx, conn)
		}
		methodDesc, ok := conn.Spec().Schema.(protoreflect.MethodDescriptor)
		if assert.True(a.tb, ok) {
			procedure := fmt.Sprintf("/%s/%s", methodDesc.Parent().FullName(), methodDesc.Name())
			assert.Equal(a.tb, procedure, conn.Spec().Procedure)
		}
		return next(ctx, conn)
	}
}

type rpcErrors struct {
	sendErr      error
	recvErr      error
	closeSendErr error
	closeRecvErr error
}

func addUnrecognizedBytes[M proto.Message](msg M, data []byte) M {
	msg.ProtoReflect().SetUnknown(data)
	return msg
}

type httpClientFunc func(*http.Request) (*http.Response, error)

func (fn httpClientFunc) Do(req *http.Request) (*http.Response, error) {
	return fn(req)
}
