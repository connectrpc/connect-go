// Copyright 2021-2025 The Connect Authors
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
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	connect "connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/generics/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/internal/memhttp"
	"connectrpc.com/connect/internal/memhttp/memhttptest"
)

func TestSideQuestInInterceptor(t *testing.T) {
	t.Parallel()
	t.Run("unary", func(t *testing.T) {
		t.Parallel()
		t.Run("sidequest_succeeds", func(t *testing.T) {
			t.Parallel()
			createInterceptors := func(clientCounter1 *atomic.Int32, clientCounter2 *atomic.Int32, server *memhttp.Server) connect.Option {
				return connect.WithInterceptors(
					newSideQuestInterceptor(t, clientCounter1, server),
					newSideQuestInterceptor(t, clientCounter2, server),
				)
			}
			var clientCounter1, clientCounter2 atomic.Int32
			mux := http.NewServeMux()
			mux.Handle(
				pingv1connect.NewPingServiceHandler(
					pingServer{},
				),
			)
			server := memhttptest.NewServer(t, mux)
			client := pingv1connect.NewPingServiceClient(
				server.Client(),
				server.URL(),
				createInterceptors(&clientCounter1, &clientCounter2, server),
			)
			_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{Number: 10}))

			assert.Nil(t, err)
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
		})
	})
}

func TestNewClientContextFails(t *testing.T) {
	// Verifies that calling NewClientContext in an interceptor fails when sending the new context downstream
	t.Parallel()
	t.Run("unary", func(t *testing.T) {
		t.Parallel()
		t.Run("first_interceptor", func(t *testing.T) {
			t.Parallel()
			createInterceptors := func(clientCounter1 *atomic.Int32, clientCounter2 *atomic.Int32) connect.Option {
				return connect.WithInterceptors(
					&contextInterceptor{client: true, count: clientCounter1, createNewContext: true},
					&contextInterceptor{client: true, count: clientCounter2},
				)
			}
			var clientCounter1, clientCounter2 atomic.Int32
			mux := http.NewServeMux()
			mux.Handle(
				pingv1connect.NewPingServiceHandler(
					pingServer{},
				),
			)
			server := memhttptest.NewServer(t, mux)
			client := pingv1connect.NewPingServiceClient(
				server.Client(),
				server.URL(),
				createInterceptors(&clientCounter1, &clientCounter2),
			)
			_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{Number: 10}))

			// Since we are creating a new client context, an error will be returned from the invocation
			assert.NotNil(t, err)
			assert.Equal(t, err.Error(), "creating a new context in an interceptor is prohibited")
			// And because we're creating it in the first interceptor, only the first interceptor fires
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(0), clientCounter2.Load())
		})
		t.Run("subsequent_interceptor", func(t *testing.T) {
			t.Parallel()
			createInterceptors := func(clientCounter1 *atomic.Int32, clientCounter2 *atomic.Int32) connect.Option {
				return connect.WithInterceptors(
					&contextInterceptor{client: true, count: clientCounter1},
					&contextInterceptor{client: true, count: clientCounter2, createNewContext: true},
				)
			}
			var clientCounter1, clientCounter2 atomic.Int32
			mux := http.NewServeMux()
			mux.Handle(
				pingv1connect.NewPingServiceHandler(
					pingServer{},
				),
			)
			server := memhttptest.NewServer(t, mux)
			client := pingv1connect.NewPingServiceClient(
				server.Client(),
				server.URL(),
				createInterceptors(&clientCounter1, &clientCounter2),
			)
			_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{Number: 10}))

			// Since we are creating a new client context, an error will be returned from the invocation
			assert.NotNil(t, err)
			assert.Equal(t, err.Error(), "creating a new context in an interceptor is prohibited")
			// And because we're creating it in the second interceptor, they both fire
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
		})
	})
	t.Run("server_streaming", func(t *testing.T) {
		t.Parallel()
		t.Run("first_interceptor", func(t *testing.T) {
			t.Parallel()
			createInterceptors := func(clientCounter1 *atomic.Int32, clientCounter2 *atomic.Int32) connect.Option {
				return connect.WithInterceptors(
					&contextInterceptor{client: true, count: clientCounter1, createNewContext: true},
					&contextInterceptor{client: true, count: clientCounter2},
				)
			}
			var clientCounter1, clientCounter2 atomic.Int32
			mux := http.NewServeMux()
			mux.Handle(
				pingv1connect.NewPingServiceHandler(
					pingServer{},
				),
			)
			server := memhttptest.NewServer(t, mux)
			client := pingv1connect.NewPingServiceClient(
				server.Client(),
				server.URL(),
				createInterceptors(&clientCounter1, &clientCounter2),
			)
			responses, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{Number: 10}))

			// Since we are creating a new client context, an error will be returned from the invocation
			assert.Nil(t, responses)
			assert.NotNil(t, err)
			assert.Equal(t, err.Error(), "creating a new context in an interceptor is prohibited")
			// And because we're creating it in the first interceptor, only the first interceptor fires
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(0), clientCounter2.Load())
		})
		t.Run("subsequent_interceptor", func(t *testing.T) {
			t.Parallel()
			createInterceptors := func(clientCounter1 *atomic.Int32, clientCounter2 *atomic.Int32) connect.Option {
				return connect.WithInterceptors(
					&contextInterceptor{client: true, count: clientCounter1},
					&contextInterceptor{client: true, count: clientCounter2, createNewContext: true},
				)
			}
			var clientCounter1, clientCounter2 atomic.Int32
			mux := http.NewServeMux()
			mux.Handle(
				pingv1connect.NewPingServiceHandler(
					pingServer{},
				),
			)
			server := memhttptest.NewServer(t, mux)
			client := pingv1connect.NewPingServiceClient(
				server.Client(),
				server.URL(),
				createInterceptors(&clientCounter1, &clientCounter2),
			)
			responses, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{Number: 10}))

			// Since we are creating a new client context, an error will be returned from the invocation
			assert.Nil(t, responses)
			assert.NotNil(t, err)
			assert.Equal(t, err.Error(), "creating a new context in an interceptor is prohibited")
			// And because we're creating it in the second interceptor, all interceptors fire
			assert.Equal(t, int32(1), clientCounter1.Load())
			assert.Equal(t, int32(1), clientCounter2.Load())
		})
	})
}

func TestOnionOrderingEndToEnd(t *testing.T) {
	t.Parallel()
	// Helper function: returns a function that asserts that there's some value
	// set for header "expect", and adds a value for header "add".
	newInspector := func(expect, add string) func(connect.Spec, http.Header) {
		return func(spec connect.Spec, header http.Header) {
			if expect != "" {
				assert.NotZero(
					t,
					header.Get(expect),
					assert.Sprintf(
						"%s (IsClient %v): header %q missing: %v",
						spec.Procedure,
						spec.IsClient,
						expect,
						header,
					),
				)
			}
			header.Set(add, "v")
		}
	}
	// Helper function: asserts that there's a value present for header keys
	// "one", "two", "three", and "four".
	assertAllPresent := func(spec connect.Spec, header http.Header) {
		for _, key := range []string{"one", "two", "three", "four"} {
			assert.NotZero(
				t,
				header.Get(key),
				assert.Sprintf(
					"%s (IsClient %v): checking all headers, %q missing: %v",
					spec.Procedure,
					spec.IsClient,
					key,
					header,
				),
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
	clientOnion := connect.WithInterceptors(
		newHeaderInterceptor(
			&clientCounter1,
			// 1 (start). request: should see protocol-related headers
			func(_ connect.Spec, h http.Header) {
				assert.NotZero(t, h.Get("Content-Type"))
			},
			// 12 (end). response: check "one"-"four"
			assertAllPresent,
		),
		newHeaderInterceptor(
			&clientCounter2,
			newInspector("", "one"),       // 2. request: add header "one"
			newInspector("three", "four"), // 11. response: check "three", add "four"
		),
		newHeaderInterceptor(
			&clientCounter3,
			newInspector("one", "two"),   // 3. request: check "one", add "two"
			newInspector("two", "three"), // 10. response: check "two", add "three"
		),
	)
	handlerOnion := connect.WithInterceptors(
		newHeaderInterceptor(
			&handlerCounter1,
			newInspector("two", "three"), // 4. request: check "two", add "three"
			newInspector("one", "two"),   // 9. response: check "one", add "two"
		),
		newHeaderInterceptor(
			&handlerCounter2,
			newInspector("three", "four"), // 5. request: check "three", add "four"
			newInspector("", "one"),       // 8. response: add "one"
		),
		newHeaderInterceptor(
			&handlerCounter3,
			assertAllPresent, // 6. request: check "one"-"four"
			nil,              // 7. response: no-op
		),
	)

	mux := http.NewServeMux()
	mux.Handle(
		pingv1connect.NewPingServiceHandler(
			pingServer{},
			handlerOnion,
		),
	)
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL(),
		clientOnion,
	)

	_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{Number: 10}))
	assert.Nil(t, err)

	// make sure the interceptors were actually invoked
	assert.Equal(t, int32(1), clientCounter1.Load())
	assert.Equal(t, int32(1), clientCounter2.Load())
	assert.Equal(t, int32(1), clientCounter3.Load())
	assert.Equal(t, int32(1), handlerCounter1.Load())
	assert.Equal(t, int32(1), handlerCounter2.Load())
	assert.Equal(t, int32(1), handlerCounter3.Load())

	responses, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{Number: 10}))
	assert.Nil(t, err)
	var sum int64
	for responses.Receive() {
		sum += responses.Msg().GetNumber()
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
	interceptor := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
			return next(ctx, request)
		}
	})
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}, connect.WithInterceptors(interceptor)))
	server := memhttptest.NewServer(t, mux)
	connectClient := pingv1connect.NewPingServiceClient(server.Client(), server.URL(), connect.WithInterceptors(interceptor))
	_, err := connectClient.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{}))
	assert.Nil(t, err)
	sumStream := connectClient.Sum(context.Background())
	assert.Nil(t, sumStream.Send(&pingv1.SumRequest{Number: 1}))
	resp, err := sumStream.CloseAndReceive()
	assert.Nil(t, err)
	assert.NotNil(t, resp)
	countUpStream, err := connectClient.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{}))
	assert.Nil(t, err)
	for countUpStream.Receive() {
		assert.NotNil(t, countUpStream.Msg())
	}
	assert.Nil(t, countUpStream.Close())
}

func TestInterceptorFuncAccessingHTTPMethod(t *testing.T) {
	t.Parallel()
	clientChecker := &httpMethodChecker{client: true}
	handlerChecker := &httpMethodChecker{}

	mux := http.NewServeMux()
	mux.Handle(
		pingv1connect.NewPingServiceHandler(
			pingServer{},
			connect.WithInterceptors(handlerChecker),
		),
	)
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL(),
		connect.WithInterceptors(clientChecker),
	)

	pingReq := connect.NewRequest(&pingv1.PingRequest{Number: 10})
	assert.Equal(t, "", pingReq.HTTPMethod())
	_, err := client.Ping(context.Background(), pingReq)
	assert.Nil(t, err)
	assert.Equal(t, http.MethodPost, pingReq.HTTPMethod())

	// make sure interceptor was invoked
	assert.Equal(t, int32(1), clientChecker.count.Load())
	assert.Equal(t, int32(1), handlerChecker.count.Load())

	countUpReq := connect.NewRequest(&pingv1.CountUpRequest{Number: 10})
	assert.Equal(t, "", countUpReq.HTTPMethod())
	responses, err := client.CountUp(context.Background(), countUpReq)
	assert.Nil(t, err)
	var sum int64
	for responses.Receive() {
		sum += responses.Msg().GetNumber()
	}
	assert.Equal(t, sum, 55)
	assert.Nil(t, responses.Close())
	assert.Equal(t, http.MethodPost, countUpReq.HTTPMethod())

	// make sure interceptor was invoked again
	assert.Equal(t, int32(2), clientChecker.count.Load())
	assert.Equal(t, int32(2), handlerChecker.count.Load())
}

// headerInterceptor makes it easier to write interceptors that inspect or
// mutate HTTP headers. It applies the same logic to unary and streaming
// procedures, wrapping the send or receive side of the stream as appropriate.
//
// It's useful as a testing harness to make sure that we're chaining
// interceptors in the correct order.
type headerInterceptor struct {
	counter               *atomic.Int32
	inspectRequestHeader  func(connect.Spec, http.Header)
	inspectResponseHeader func(connect.Spec, http.Header)
}

// newHeaderInterceptor constructs a headerInterceptor. Nil function pointers
// are treated as no-ops.
func newHeaderInterceptor(
	counter *atomic.Int32,
	inspectRequestHeader func(connect.Spec, http.Header),
	inspectResponseHeader func(connect.Spec, http.Header),
) *headerInterceptor {
	interceptor := headerInterceptor{
		counter:               counter,
		inspectRequestHeader:  inspectRequestHeader,
		inspectResponseHeader: inspectResponseHeader,
	}
	if interceptor.inspectRequestHeader == nil {
		interceptor.inspectRequestHeader = func(_ connect.Spec, _ http.Header) {}
	}
	if interceptor.inspectResponseHeader == nil {
		interceptor.inspectResponseHeader = func(_ connect.Spec, _ http.Header) {}
	}
	return &interceptor
}

func (h *headerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		h.counter.Add(1)
		h.inspectRequestHeader(req.Spec(), req.Header())
		res, err := next(ctx, req)
		if err != nil {
			return nil, err
		}
		h.inspectResponseHeader(req.Spec(), res.Header())
		return res, nil
	}
}

func (h *headerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		h.counter.Add(1)
		return &headerInspectingClientConn{
			StreamingClientConn:   next(ctx, spec),
			inspectRequestHeader:  h.inspectRequestHeader,
			inspectResponseHeader: h.inspectResponseHeader,
		}
	}
}

func (h *headerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		h.counter.Add(1)
		h.inspectRequestHeader(conn.Spec(), conn.RequestHeader())
		return next(ctx, &headerInspectingHandlerConn{
			StreamingHandlerConn:  conn,
			inspectResponseHeader: h.inspectResponseHeader,
		})
	}
}

type headerInspectingHandlerConn struct {
	connect.StreamingHandlerConn

	inspectedResponse     bool
	inspectResponseHeader func(connect.Spec, http.Header)
}

func (hc *headerInspectingHandlerConn) Send(msg any) error {
	if !hc.inspectedResponse {
		hc.inspectResponseHeader(hc.Spec(), hc.ResponseHeader())
		hc.inspectedResponse = true
	}
	return hc.StreamingHandlerConn.Send(msg)
}

type headerInspectingClientConn struct {
	connect.StreamingClientConn

	inspectedRequest      bool
	inspectRequestHeader  func(connect.Spec, http.Header)
	inspectedResponse     bool
	inspectResponseHeader func(connect.Spec, http.Header)
}

func (cc *headerInspectingClientConn) Send(msg any) error {
	if !cc.inspectedRequest {
		cc.inspectRequestHeader(cc.Spec(), cc.RequestHeader())
		cc.inspectedRequest = true
	}
	return cc.StreamingClientConn.Send(msg)
}

func (cc *headerInspectingClientConn) Receive(msg any) error {
	err := cc.StreamingClientConn.Receive(msg)
	if !cc.inspectedResponse {
		cc.inspectResponseHeader(cc.Spec(), cc.ResponseHeader())
		cc.inspectedResponse = true
	}
	return err
}

type httpMethodChecker struct {
	client bool
	count  atomic.Int32
}

func (h *httpMethodChecker) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		h.count.Add(1)
		if h.client {
			// should be blank until after we make request
			if req.HTTPMethod() != "" {
				return nil, fmt.Errorf("expected blank HTTP method but instead got %q", req.HTTPMethod())
			}
		} else {
			// server interceptors see method from the start
			// NB: In theory, the method could also be GET, not just POST. But for the
			// configuration under test, it will always be POST.
			if req.HTTPMethod() != http.MethodPost {
				return nil, fmt.Errorf("expected HTTP method %s but instead got %q", http.MethodPost, req.HTTPMethod())
			}
		}
		resp, err := next(ctx, req)
		// NB: In theory, the method could also be GET, not just POST. But for the
		// configuration under test, it will always be POST.
		if req.HTTPMethod() != http.MethodPost {
			return nil, fmt.Errorf("expected HTTP method %s but instead got %q", http.MethodPost, req.HTTPMethod())
		}
		return resp, err
	}
}

func (h *httpMethodChecker) WrapStreamingClient(clientFunc connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		// method not exposed to streaming interceptor, but that's okay because it's always POST for streams
		h.count.Add(1)
		return clientFunc(ctx, spec)
	}
}

func (h *httpMethodChecker) WrapStreamingHandler(handlerFunc connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		// method not exposed to streaming interceptor, but that's okay because it's always POST for streams
		h.count.Add(1)
		return handlerFunc(ctx, conn)
	}
}

type contextInterceptor struct {
	client bool
	count  *atomic.Int32
	// Whether the interceptor should attempt to create a new context (which will cause next() to return an error)
	createNewContext bool
}

func (h *contextInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		h.count.Add(1)
		if h.createNewContext {
			// This will cause next to return an error
			ctx, _ = connect.NewClientContext(ctx)
		}
		return next(ctx, req)
	}
}

func (h *contextInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		h.count.Add(1)
		if h.createNewContext {
			// This will cause next to return an error
			ctx, _ = connect.NewClientContext(ctx)
		}
		return next(ctx, spec)
	}
}

func (h *contextInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		h.count.Add(1)
		return next(ctx, conn)
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
	client := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL(),
	)
	return &sideQuestInterceptor{t: t, client: client, count: counter}
}

func (h *sideQuestInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		h.count.Add(1)
		num := int64(42)
		// Create a new client context for the side quest Ping. This should succeed because we aren't
		// sending this on through the interceptor chain and reusing this context
		newCtx, _ := connect.NewClientContext(ctx)
		resp, err := h.client.Ping(newCtx, connect.NewRequest(&pingv1.PingRequest{Number: num}))
		assert.Nil(h.t, err)
		assert.Equal(h.t, resp.Msg.Number, num)

		return next(ctx, req)
	}
}

func (h *sideQuestInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		h.count.Add(1)
		// Create a new context for the side quest CountUp. This should succeed because we aren't
		// sending this on through the interceptor chain and reusing this context
		newCtx, _ := connect.NewClientContext(ctx)
		responses, err := h.client.CountUp(newCtx, connect.NewRequest(&pingv1.CountUpRequest{Number: 3}))
		assert.Nil(h.t, err)
		var sum int64
		for responses.Receive() {
			sum += responses.Msg().GetNumber()
		}
		assert.Equal(h.t, sum, 6)
		assert.Nil(h.t, responses.Close())
		return next(ctx, spec)
	}
}

func (h *sideQuestInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return next(ctx, conn)
	}
}
