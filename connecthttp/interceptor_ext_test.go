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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/memhttp"
	"connectrpc.com/connect/v2/internal/memhttp/memhttptest"
)

func TestNewClientContextInInterceptor(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()

	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)

	server := memhttptest.NewServer(t, mux)
	t.Run("first_interceptor", func(t *testing.T) {
		t.Parallel()
		createClient := func(counter1 *atomic.Int32, counter2 *atomic.Int32) pingv1connect.PingServiceClient {
			return pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
				server.URL()),
				(&contextInterceptor{count: counter1, createNewContext: true}).ClientInterceptor,
				(&contextInterceptor{count: counter2}).ClientInterceptor,
			))
		}
		t.Run("unary", func(t *testing.T) {
			t.Parallel()
			var clientCounter1, clientCounter2 atomic.Int32
			client := createClient(&clientCounter1, &clientCounter2)
			resp, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 10})
			assert.Nil(t, err)
			assert.NotNil(t, resp)
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
		})
		t.Run("server_stream", func(t *testing.T) {
			t.Parallel()
			var clientCounter1, clientCounter2 atomic.Int32
			client := createClient(&clientCounter1, &clientCounter2)
			stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 10})
			assert.Nil(t, err)
			assert.NotNil(t, stream)
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
			assert.Nil(t, stream.Close())
		})
		t.Run("client_stream", func(t *testing.T) {
			t.Parallel()
			var clientCounter1, clientCounter2 atomic.Int32
			client := createClient(&clientCounter1, &clientCounter2)
			stream, err := client.Sum(t.Context())
			assert.Nil(t, err)
			assert.NotNil(t, stream)
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
			assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 1}))
			resp, err := stream.CloseAndReceive()
			assert.Nil(t, err)
			assert.NotNil(t, resp)
		})
		t.Run("bidi_stream", func(t *testing.T) {
			t.Parallel()
			var clientCounter1, clientCounter2 atomic.Int32
			client := createClient(&clientCounter1, &clientCounter2)
			stream, err := client.CumSum(t.Context())
			assert.Nil(t, err)
			assert.NotNil(t, stream)
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
			assert.Nil(t, stream.CloseSend())
			assert.Nil(t, stream.Close())
		})
	})
	t.Run("subsequent_interceptor", func(t *testing.T) {
		t.Parallel()
		createClient := func(counter1 *atomic.Int32, counter2 *atomic.Int32) pingv1connect.PingServiceClient {
			return pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
				server.URL()),
				(&contextInterceptor{count: counter1}).ClientInterceptor,
				(&contextInterceptor{count: counter2, createNewContext: true}).ClientInterceptor,
			))
		}
		t.Run("unary", func(t *testing.T) {
			t.Parallel()
			var clientCounter1, clientCounter2 atomic.Int32
			client := createClient(&clientCounter1, &clientCounter2)
			resp, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 10})
			assert.Nil(t, err)
			assert.NotNil(t, resp)
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
		})
		t.Run("server_stream", func(t *testing.T) {
			t.Parallel()
			var clientCounter1, clientCounter2 atomic.Int32
			client := createClient(&clientCounter1, &clientCounter2)
			stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 10})
			assert.Nil(t, err)
			assert.NotNil(t, stream)
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
			assert.Nil(t, stream.Close())
		})
		t.Run("client_stream", func(t *testing.T) {
			t.Parallel()
			var clientCounter1, clientCounter2 atomic.Int32
			client := createClient(&clientCounter1, &clientCounter2)
			stream, err := client.Sum(t.Context())
			assert.Nil(t, err)
			assert.NotNil(t, stream)
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
			assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 1}))
			resp, err := stream.CloseAndReceive()
			assert.Nil(t, err)
			assert.NotNil(t, resp)
		})
		t.Run("bidi_stream", func(t *testing.T) {
			t.Parallel()
			var clientCounter1, clientCounter2 atomic.Int32
			client := createClient(&clientCounter1, &clientCounter2)
			stream, err := client.CumSum(t.Context())
			assert.Nil(t, err)
			assert.NotNil(t, stream)
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
			assert.Nil(t, stream.CloseSend())
			assert.Nil(t, stream.Close())
		})
	})
	t.Run("sidequest_succeeds", func(t *testing.T) {
		t.Parallel()
		// These tests create a new context but it is used to issue a separate/new request and not reused in the
		// interceptor chain. So, all interceptors should fire and no errors should be returned.
		createClient := func(counter1 *atomic.Int32, counter2 *atomic.Int32) pingv1connect.PingServiceClient {
			opts := []connect.ClientInterceptor{
				newSideQuestInterceptor(t, counter1, server).ClientInterceptor,
				newSideQuestInterceptor(t, counter2, server).ClientInterceptor,
			}
			return pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
				server.URL()), opts...),
			)
		}
		t.Run("unary", func(t *testing.T) {
			t.Parallel()
			var clientCounter1, clientCounter2 atomic.Int32
			client := createClient(&clientCounter1, &clientCounter2)

			resp, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 10})
			assert.NotNil(t, resp)
			assert.Nil(t, err)
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
		})
		t.Run("server_stream", func(t *testing.T) {
			t.Parallel()
			var clientCounter1, clientCounter2 atomic.Int32
			client := createClient(&clientCounter1, &clientCounter2)

			stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 10})
			assert.NotNil(t, stream)
			assert.Nil(t, err)
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
			assert.Nil(t, stream.Close())
		})
		t.Run("client_stream", func(t *testing.T) {
			t.Parallel()
			var clientCounter1, clientCounter2 atomic.Int32
			client := createClient(&clientCounter1, &clientCounter2)

			stream, err := client.Sum(t.Context())
			assert.NotNil(t, stream)
			assert.Nil(t, err)
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
			resp, err := stream.CloseAndReceive()
			assert.Nil(t, err)
			assert.NotNil(t, resp)
		})
		t.Run("bidi_stream", func(t *testing.T) {
			t.Parallel()
			var clientCounter1, clientCounter2 atomic.Int32
			client := createClient(&clientCounter1, &clientCounter2)

			stream, err := client.CumSum(t.Context())
			assert.Nil(t, err)
			assert.NotNil(t, stream)

			assert.Nil(t, stream.CloseSend())
			assert.Nil(t, stream.Close())
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
		})
	})
}

func TestOnionOrderingEndToEnd(t *testing.T) {
	t.Parallel()
	// Helper function: returns a function that asserts that there's some value
	// set for header "expect", and adds a value for header "add".
	newInspector := func(expect, add string) func(connect.Spec, *connect.Header) {
		return func(spec connect.Spec, header *connect.Header) {
			if expect != "" {
				assert.True(
					t,
					header.Has(expect),
					assert.Sprintf("%s: header %q missing: %v", spec.Procedure, expect, header),
				)
			}
			header.Set(add, "v")
		}
	}
	// Helper function: asserts that there's a value present for header keys
	// "one", "two", "three", and "four".
	assertAllPresent := func(spec connect.Spec, header *connect.Header) {
		for _, key := range []string{"one", "two", "three", "four"} {
			assert.True(
				t,
				header.Has(key),
				assert.Sprintf("%s: checking all headers, %q missing: %v", spec.Procedure, key, header),
			)
		}
	}

	var clientCounter1, clientCounter2, clientCounter3, handlerCounter1, handlerCounter2, handlerCounter3 atomic.Int32

	// The client and handler interceptor onions are the meat of the test. The
	// order of interceptor execution must be the same for unary and streaming
	// procedures.
	//
	// Requests should fall through the client onion from top to bottom, traverse
	// the network, and then fall through the handler onion from top to bottom.
	// Responses should climb up the handler onion, traverse the network, and
	// then climb up the client onion.
	//
	// The request and response sides of this onion are numbered to make the
	// intended order clear.
	clientOnion1 := newHeaderInterceptor(
		&clientCounter1,
		nil,              // 1 (start). request: no-op
		assertAllPresent, // 12 (end). response: check "one"-"four"
	)
	clientOnion2 := newHeaderInterceptor(
		&clientCounter2,
		newInspector("", "one"),       // 2. request: add header "one"
		newInspector("three", "four"), // 11. response: check "three", add "four"
	)
	clientOnion3 := newHeaderInterceptor(
		&clientCounter3,
		newInspector("one", "two"),   // 3. request: check "one", add "two"
		newInspector("two", "three"), // 10. response: check "two", add "three"
	)
	handlerOnion1 := newHeaderInterceptor(
		&handlerCounter1,
		newInspector("two", "three"), // 4. request: check "two", add "three"
		newInspector("one", "two"),   // 9. response: check "one", add "two"
	)
	handlerOnion2 := newHeaderInterceptor(
		&handlerCounter2,
		newInspector("three", "four"), // 5. request: check "three", add "four"
		newInspector("", "one"),       // 8. response: add "one"
	)
	handlerOnion3 := newHeaderInterceptor(
		&handlerCounter3,
		assertAllPresent, // 6. request: check "one"-"four"
		nil,              // 7. response: no-op
	)

	mux := http.NewServeMux()
	srv := connect.NewServer(
		handlerOnion1.ServerInterceptor,
		handlerOnion2.ServerInterceptor,
		handlerOnion3.ServerInterceptor,
	)
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)

	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
		server.URL()),
		clientOnion1.ClientInterceptor,
		clientOnion2.ClientInterceptor,
		clientOnion3.ClientInterceptor,
	))

	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 10})
	assert.Nil(t, err)

	// make sure the interceptors were actually invoked
	assert.Equal(t, int32(1), clientCounter1.Load())
	assert.Equal(t, int32(1), clientCounter2.Load())
	assert.Equal(t, int32(1), clientCounter3.Load())
	assert.Equal(t, int32(1), handlerCounter1.Load())
	assert.Equal(t, int32(1), handlerCounter2.Load())
	assert.Equal(t, int32(1), handlerCounter3.Load())

	responses, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 10})
	assert.Nil(t, err)
	var sum int64
	for {
		msg, err := responses.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		sum += msg.GetNumber()
	}
	assert.Equal(t, sum, 55)
	assert.Nil(t, responses.Close())

	// make sure the interceptors were invoked again
	assert.Equal(t, int32(2), clientCounter1.Load())
	assert.Equal(t, int32(2), clientCounter2.Load())
	assert.Equal(t, int32(2), clientCounter3.Load())
	assert.Equal(t, int32(2), handlerCounter1.Load())
	assert.Equal(t, int32(2), handlerCounter2.Load())
	assert.Equal(t, int32(2), handlerCounter3.Load())
}

func TestEmptyUnaryInterceptorFunc(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	clientInterceptor := func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			return next(ctx, spec)
		}
	}
	serverInterceptor := func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			return next(ctx, spec, stream)
		}
	}
	srv := connect.NewServer(serverInterceptor)
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)
	connectClient := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL()), clientInterceptor))
	_, err := connectClient.Ping(t.Context(), &pingv1.PingRequest{})
	assert.Nil(t, err)
	sumStream, err := connectClient.Sum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assert.Nil(t, sumStream.Send(&pingv1.SumRequest{Number: 1}))
	resp, err := sumStream.CloseAndReceive()
	assert.Nil(t, err)
	assert.NotNil(t, resp)
	countUpStream, err := connectClient.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 1})
	assert.Nil(t, err)
	for {
		msg, err := countUpStream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		assert.NotNil(t, msg)
	}
	assert.Nil(t, countUpStream.Close())
}

func TestReusedCallInfoResetsTransportFields(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)

	assertFresh := func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			info, _ := connect.CallInfoForClientContext(ctx)
			assert.Nil(t, info.TransportInfo)
			assert.Equal(t, info.Codec, "")
			assert.Equal(t, info.PeerAddr, "")
			assert.Equal(t, info.ResponseHeader().Len(), 0)
			assert.Equal(t, info.ResponseTrailer().Len(), 0)
			return next(ctx, spec)
		}
	}
	client := pingv1connect.NewPingServiceClient(connect.NewClient(
		connecthttp.NewTransport(server.Client(), server.URL()),
		assertFresh,
	))

	// Both calls share one ctx, so the second reuses the first call's
	// CallInfo, and the interceptor asserts it was reset at dispatch.
	ctx, info := connect.NewClientContext(t.Context())
	for range 2 {
		_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 1})
		assert.Nil(t, err)
		assert.NotNil(t, info.TransportInfo)
		assert.NotEqual(t, info.Codec, "")
	}
}

func TestInterceptorFuncAccessingHTTPMethod(t *testing.T) {
	t.Parallel()
	clientChecker := &httpMethodChecker{}
	handlerChecker := &httpMethodChecker{}

	mux := http.NewServeMux()
	srv := connect.NewServer(handlerChecker.ServerInterceptor)
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)

	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
		server.URL()), clientChecker.ClientInterceptor),
	)

	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 10})
	assert.Nil(t, err)

	// make sure interceptor was invoked
	assert.Equal(t, int32(1), clientChecker.count.Load())
	assert.Equal(t, int32(1), handlerChecker.count.Load())

	responses, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 10})
	assert.Nil(t, err)
	var sum int64
	for {
		msg, err := responses.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		sum += msg.GetNumber()
	}
	assert.Equal(t, sum, 55)
	assert.Nil(t, responses.Close())

	// make sure interceptor was invoked again
	assert.Equal(t, int32(2), clientChecker.count.Load())
	assert.Equal(t, int32(2), handlerChecker.count.Load())
}

func TestHandlerErrorResponseNilInInterceptor(t *testing.T) {
	t.Parallel()
	handlerErr := connect.NewError(connect.CodeInternal, "handler error")
	var sawNilResponse atomic.Bool
	interceptor := func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			stream, err := next(ctx, spec)
			if err != nil {
				return nil, err
			}
			return &nilResponseInspector{ClientStream: stream, sawNil: &sawNilResponse}, nil
		}
	}
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, &pluggablePingServer{
		ping: func(context.Context, *pingv1.PingRequest) (*pingv1.PingResponse, error) {
			return nil, handlerErr
		},
	})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(
		connecthttp.NewTransport(server.Client(), server.URL()), interceptor,
	))
	_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
	assert.NotNil(t, err)
	assert.True(t, sawNilResponse.Load())
}

// nilResponseInspector records that no response message was received, which
// happens when the handler returns an error.
type nilResponseInspector struct {
	connect.ClientStream

	sawNil *atomic.Bool
}

func (s *nilResponseInspector) Receive(msg any) error {
	err := s.ClientStream.Receive(msg)
	if err != nil {
		s.sawNil.Store(true)
	}
	return err
}

// headerInterceptor makes it easier to write interceptors that inspect or
// mutate HTTP headers. It applies the same logic to unary and streaming
// procedures, wrapping the send or receive side of the stream as appropriate.
//
// It's useful as a testing harness to make sure that we're chaining
// interceptors in the correct order.
type headerInterceptor struct {
	counter               *atomic.Int32
	inspectRequestHeader  func(connect.Spec, *connect.Header)
	inspectResponseHeader func(connect.Spec, *connect.Header)
}

// newHeaderInterceptor constructs a headerInterceptor. Nil function pointers
// are treated as no-ops.
func newHeaderInterceptor(
	counter *atomic.Int32,
	inspectRequestHeader func(connect.Spec, *connect.Header),
	inspectResponseHeader func(connect.Spec, *connect.Header),
) *headerInterceptor {
	interceptor := headerInterceptor{
		counter:               counter,
		inspectRequestHeader:  inspectRequestHeader,
		inspectResponseHeader: inspectResponseHeader,
	}
	if interceptor.inspectRequestHeader == nil {
		interceptor.inspectRequestHeader = func(_ connect.Spec, _ *connect.Header) {}
	}
	if interceptor.inspectResponseHeader == nil {
		interceptor.inspectResponseHeader = func(_ connect.Spec, _ *connect.Header) {}
	}
	return &interceptor
}

func (h *headerInterceptor) ClientInterceptor(next connect.ClientFunc) connect.ClientFunc {
	return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
		h.counter.Add(1)
		info, _ := connect.CallInfoForClientContext(ctx)
		h.inspectRequestHeader(spec, info.RequestHeader())
		stream, err := next(ctx, spec)
		if err != nil {
			return nil, err
		}
		return &clientStreamInspector{ClientStream: stream, inspect: func() error {
			h.inspectResponseHeader(spec, info.ResponseHeader())
			return nil
		}}, nil
	}
}

type clientStreamInspector struct {
	connect.ClientStream

	inspect func() error
}

func (s *clientStreamInspector) Close() error {
	err := s.ClientStream.Close()
	if inspectErr := s.inspect(); inspectErr != nil {
		return inspectErr
	}
	return err
}

func (h *headerInterceptor) ServerInterceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		h.counter.Add(1)
		info, _ := connect.CallInfoForServerContext(ctx)
		h.inspectRequestHeader(spec, info.RequestHeader())
		return next(ctx, spec, &headerInspectingHandlerConn{
			ServerStream:          stream,
			ctx:                   ctx,
			spec:                  spec,
			inspectResponseHeader: h.inspectResponseHeader,
		})
	}
}

type headerInspectingHandlerConn struct {
	connect.ServerStream

	ctx                   context.Context
	spec                  connect.Spec
	inspectedResponse     bool
	inspectResponseHeader func(connect.Spec, *connect.Header)
}

func (hc *headerInspectingHandlerConn) Send(msg any) error {
	if !hc.inspectedResponse {
		info, _ := connect.CallInfoForServerContext(hc.ctx)
		hc.inspectResponseHeader(hc.spec, info.ResponseHeader())
		hc.inspectedResponse = true
	}
	return hc.ServerStream.Send(msg)
}

type httpMethodChecker struct {
	count atomic.Int32
}

func (h *httpMethodChecker) ClientInterceptor(next connect.ClientFunc) connect.ClientFunc {
	return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
		h.count.Add(1)
		// method not exposed to streaming interceptor, but that's okay because it's always POST for streams
		if spec.StreamType != connect.StreamTypeUnary {
			return next(ctx, spec)
		}
		callInfo, _ := connect.CallInfoForClientContext(ctx)
		// TransportInfo is not set until the transport opens the stream.
		if httpInfo, ok := callInfo.TransportInfo.(*connecthttp.ClientInfo); ok && httpInfo.HTTPMethod() != "" {
			return nil, fmt.Errorf("expected blank HTTP method but instead got %q", httpInfo.HTTPMethod())
		}
		stream, err := next(ctx, spec)
		if err != nil {
			return nil, err
		}
		return &clientStreamInspector{ClientStream: stream, inspect: func() error {
			// NB: In theory, the method could also be GET, not just POST. But for the
			// configuration under test, it will always be POST.
			httpInfo, _ := callInfo.TransportInfo.(*connecthttp.ClientInfo)
			if httpInfo.HTTPMethod() != http.MethodPost {
				return fmt.Errorf("expected HTTP method %s but instead got %q", http.MethodPost, httpInfo.HTTPMethod())
			}
			return nil
		}}, nil
	}
}

func (h *httpMethodChecker) ServerInterceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		h.count.Add(1)
		// method not exposed to streaming interceptor, but that's okay because it's always POST for streams
		if spec.StreamType == connect.StreamTypeUnary {
			// server interceptors see method from the start
			// NB: In theory, the method could also be GET, not just POST. But for the
			// configuration under test, it will always be POST.
			httpInfo, _ := connecthttp.ServerInfoForContext(ctx)
			if httpInfo.HTTPMethod() != http.MethodPost {
				return fmt.Errorf("expected HTTP method %s but instead got %q", http.MethodPost, httpInfo.HTTPMethod())
			}
		}
		return next(ctx, spec, stream)
	}
}

type contextInterceptor struct {
	count *atomic.Int32
	// Whether the interceptor should derive a new context, which propagates to
	// the transport and the rest of the chain.
	createNewContext bool
}

func (h *contextInterceptor) ClientInterceptor(next connect.ClientFunc) connect.ClientFunc {
	return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
		h.count.Add(1)
		if h.createNewContext {
			// This will cause next to return an error
			ctx, _ = connect.NewClientContext(ctx)
		}
		return next(ctx, spec)
	}
}

func (h *contextInterceptor) ServerInterceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		h.count.Add(1)
		return next(ctx, spec, stream)
	}
}

type sideQuestInterceptor struct {
	count  *atomic.Int32
	client pingv1connect.PingServiceClient
	t      *testing.T
}

func newSideQuestInterceptor( //nolint:thelper
	t *testing.T,
	counter *atomic.Int32,
	server *memhttp.Server,
) *sideQuestInterceptor {
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
		server.URL())),
	)
	return &sideQuestInterceptor{t: t, client: client, count: counter}
}

func (s *sideQuestInterceptor) ClientInterceptor(next connect.ClientFunc) connect.ClientFunc {
	return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
		s.count.Add(1)
		// Create a new client context for the side quest. This should succeed because we aren't
		// sending this on through the interceptor chain and reusing this context
		newCtx, _ := connect.NewClientContext(ctx)
		if spec.StreamType == connect.StreamTypeUnary {
			num := int64(42)
			resp, err := s.client.Ping(newCtx, &pingv1.PingRequest{Number: num})
			assert.Nil(s.t, err)
			assert.Equal(s.t, resp.Number, num)
		} else {
			responses, err := s.client.CountUp(newCtx, &pingv1.CountUpRequest{Number: 3})
			assert.Nil(s.t, err)
			var sum int64
			for {
				msg, err := responses.Receive()
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					return next(ctx, spec)
				}
				sum += msg.GetNumber()
			}
			assert.Equal(s.t, sum, 6)
			assert.Nil(s.t, responses.Close())
		}
		return next(ctx, spec)
	}
}
