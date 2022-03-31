// Copyright 2021-2022 Buf Technologies, Inc.
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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	"connectrpc.com/connect/internal/gen/connect/connect/ping/v1/pingv1connect"
	pingv1 "connectrpc.com/connect/internal/gen/go/connect/ping/v1"
)

func TestClientStreamErrors(t *testing.T) {
	t.Parallel()
	_, err := pingv1connect.NewPingServiceClient(http.DefaultClient, "INVALID_URL", connect.WithGRPC())
	assert.NotNil(t, err)
	// We don't even get to calling methods on the client, so there's no question
	// of interceptors running. Once we're calling methods on the client, all
	// errors are visible to interceptors.
}

func TestHandlerStreamErrors(t *testing.T) {
	t.Parallel()
	// If we receive an HTTP request and send a response, interceptors should
	// fire - even if we can't successfully set up a stream. (This is different
	// from clients, where stream creation fails before any HTTP request is
	// issued.)
	var called bool
	reset := func() {
		called = false
	}
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(
		pingServer{},
		connect.WithInterceptors(&assertCalledInterceptor{&called}),
	))
	server := httptest.NewServer(mux)
	defer server.Close()

	t.Run("unary", func(t *testing.T) { // nolint:paralleltest
		defer reset()
		request, err := http.NewRequest(
			http.MethodPost,
			server.URL+"/connect.ping.v1.PingService/Ping",
			strings.NewReader(""),
		)
		assert.Nil(t, err)
		request.Header.Set("Content-Type", "application/grpc+proto")
		request.Header.Set("Grpc-Timeout", "INVALID")
		res, err := server.Client().Do(request)
		assert.Nil(t, err)
		assert.Equal(t, res.StatusCode, http.StatusOK)
		assert.True(t, called)
	})
	t.Run("stream", func(t *testing.T) { // nolint:paralleltest
		defer reset()
		request, err := http.NewRequest(
			http.MethodPost,
			server.URL+"/connect.ping.v1.PingService/CountUp",
			strings.NewReader(""),
		)
		assert.Nil(t, err)
		request.Header.Set("Content-Type", "application/grpc+proto")
		request.Header.Set("Grpc-Timeout", "INVALID")
		res, err := server.Client().Do(request)
		assert.Nil(t, err)
		assert.Equal(t, res.StatusCode, http.StatusOK)
		assert.True(t, called)
	})
}

func TestOnionOrderingEndToEnd(t *testing.T) {
	t.Parallel()
	// Helper function: returns a function that asserts that there's some value
	// set for header "expect", and adds a value for header "add".
	newInspector := func(expect, add string) func(connect.Specification, http.Header) {
		return func(spec connect.Specification, header http.Header) {
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
	assertAllPresent := func(spec connect.Specification, header http.Header) {
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
			// 1 (start). request: should see protocol-related headers
			func(_ connect.Specification, h http.Header) {
				assert.NotZero(t, h.Get("Grpc-Accept-Encoding"))
			},
			// 12 (end). response: check "one"-"four"
			assertAllPresent,
		),
		newHeaderInterceptor(
			newInspector("", "one"),       // 2. request: add header "one"
			newInspector("three", "four"), // 11. response: check "three", add "four"
		),
		newHeaderInterceptor(
			newInspector("one", "two"),   // 3. request: check "one", add "two"
			newInspector("two", "three"), // 10. response: check "two", add "three"
		),
	)
	handlerOnion := connect.WithInterceptors(
		newHeaderInterceptor(
			newInspector("two", "three"), // 4. request: check "two", add "three"
			newInspector("one", "two"),   // 9. response: check "one", add "two"
		),
		newHeaderInterceptor(
			newInspector("three", "four"), // 5. request: check "three", add "four"
			newInspector("", "one"),       // 8. response: add "one"
		),
		newHeaderInterceptor(
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
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := pingv1connect.NewPingServiceClient(
		server.Client(),
		server.URL,
		connect.WithGRPC(),
		clientOnion,
	)
	assert.Nil(t, err)

	_, err = client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{Number: 10}))
	assert.Nil(t, err)

	_, err = client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{Number: 10}))
	assert.Nil(t, err)
}

// headerInterceptor makes it easier to write interceptors that inspect or
// mutate HTTP headers. It applies the same logic to unary and streaming
// procedures, wrapping the send or receive side of the stream as appropriate.
//
// It's useful as a testing harness to make sure that we're chaining
// interceptors in the correct order.
type headerInterceptor struct {
	inspectRequestHeader  func(connect.Specification, http.Header)
	inspectResponseHeader func(connect.Specification, http.Header)
}

// newHeaderInterceptor constructs a headerInterceptor. Nil function pointers
// are treated as no-ops.
func newHeaderInterceptor(
	inspectRequestHeader func(connect.Specification, http.Header),
	inspectResponseHeader func(connect.Specification, http.Header),
) *headerInterceptor {
	interceptor := headerInterceptor{
		inspectRequestHeader:  inspectRequestHeader,
		inspectResponseHeader: inspectResponseHeader,
	}
	if interceptor.inspectRequestHeader == nil {
		interceptor.inspectRequestHeader = func(_ connect.Specification, _ http.Header) {}
	}
	if interceptor.inspectResponseHeader == nil {
		interceptor.inspectResponseHeader = func(_ connect.Specification, _ http.Header) {}
	}
	return &interceptor
}

func (h *headerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	call := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		h.inspectRequestHeader(req.Spec(), req.Header())
		res, err := next(ctx, req)
		if err != nil {
			return nil, err
		}
		h.inspectResponseHeader(req.Spec(), res.Header())
		return res, nil
	}
	return connect.UnaryFunc(call)
}

func (h *headerInterceptor) WrapStreamContext(ctx context.Context) context.Context {
	return ctx
}

// WrapStreamSender implements Interceptor. Depending on whether it's operating
// on a client or handler, it wraps the sender with the request- or
// response-inspecting function.
func (h *headerInterceptor) WrapStreamSender(ctx context.Context, sender connect.Sender) connect.Sender {
	if sender.Spec().IsClient {
		return &headerInspectingSender{Sender: sender, inspect: h.inspectRequestHeader}
	}
	return &headerInspectingSender{Sender: sender, inspect: h.inspectResponseHeader}
}

// WrapStreamReceiver implements Interceptor. Depending on whether it's
// operating on a client or handler, it wraps the sender with the response- or
// request-inspecting function.
func (h *headerInterceptor) WrapStreamReceiver(ctx context.Context, receiver connect.Receiver) connect.Receiver {
	if receiver.Spec().IsClient {
		return &headerInspectingReceiver{Receiver: receiver, inspect: h.inspectResponseHeader}
	}
	return &headerInspectingReceiver{Receiver: receiver, inspect: h.inspectRequestHeader}
}

type headerInspectingSender struct {
	connect.Sender

	called  bool // senders don't need to be thread-safe
	inspect func(connect.Specification, http.Header)
}

func (s *headerInspectingSender) Send(m any) error {
	if !s.called {
		s.inspect(s.Spec(), s.Header())
		s.called = true
	}
	return s.Sender.Send(m)
}

type headerInspectingReceiver struct {
	connect.Receiver

	called  bool // receivers don't need to be thread-safe
	inspect func(connect.Specification, http.Header)
}

func (r *headerInspectingReceiver) Receive(m any) error {
	if !r.called {
		r.inspect(r.Spec(), r.Header())
		r.called = true
	}
	if err := r.Receiver.Receive(m); err != nil {
		return err
	}
	return nil
}

type assertCalledInterceptor struct {
	called *bool
}

func (i *assertCalledInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return connect.UnaryFunc(
		func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			*i.called = true
			return next(ctx, req)
		},
	)
}

func (i *assertCalledInterceptor) WrapStreamContext(ctx context.Context) context.Context {
	return ctx
}

func (i *assertCalledInterceptor) WrapStreamSender(_ context.Context, sender connect.Sender) connect.Sender {
	*i.called = true
	return sender
}

func (i *assertCalledInterceptor) WrapStreamReceiver(_ context.Context, receiver connect.Receiver) connect.Receiver {
	*i.called = true
	return receiver
}
