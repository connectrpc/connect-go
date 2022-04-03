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

package connect

import "context"

// A UnaryStream sends one message and receives either a response or an error.
// Interceptors wrap UnaryStreams.
//
// The type of the request and response structs depend on the codec being used.
// When using protobuf, request.Any() and response.Any() will always be
// proto.Message implementations.
type UnaryStream interface {
	Call(context.Context, AnyRequest) (AnyResponse, error)
	Spec() Specification
}

// An Interceptor adds logic to a generated handler or client, like the
// decorators or middleware you may have seen in other libraries. Interceptors
// may replace the context, mutate the request, mutate the response, handle the
// returned error, retry, recover from panics, or do nearly anything else.
type Interceptor interface {
	// WrapUnary adds logic to a unary procedure. The returned UnaryStream must
	// be safe to call concurrently.
	WrapUnary(UnaryStream) UnaryStream

	// WrapStreamContext, WrapStreamSender, and WrapStreamReceiver work together
	// to add logic to streaming procedures. Stream interceptors work in phases.
	// First, each interceptor may wrap the request context. Then, the connect
	// runtime constructs a (Sender, Receiver) pair. Finally, each interceptor
	// may wrap the Sender and/or Receiver. For example, the flow within a
	// Handler looks like this:
	//
	//   func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	//     ctx := r.Context()
	//     if ic := h.interceptor; ic != nil {
	//       ctx = ic.WrapStreamContext(ctx)
	//     }
	//     sender, receiver := h.newStream(w, r.WithContext(ctx))
	//     if ic := h.interceptor; ic != nil {
	//       sender = ic.WrapStreamSender(ctx, sender)
	//       receiver = ic.WrapStreamReceiver(ctx, receiver)
	//     }
	//     h.serveStream(sender, receiver)
	//   }
	//
	// Sender and Receiver implementations don't need to be safe for concurrent
	// use.
	WrapStreamContext(context.Context) context.Context
	WrapStreamSender(context.Context, Sender) Sender
	WrapStreamReceiver(context.Context, Receiver) Receiver
}

// UnaryCall is an alias for the type signature of UnaryStream.Call.
//
// Unary interceptors are common, and this alias makes both the definition and
// implementations of UnaryInterceptorFunc more readable.
type UnaryCall = func(context.Context, AnyRequest) (AnyResponse, error)

// UnaryInterceptorFunc is a simple Interceptor implementation that only
// wraps unary RPCs. It has no effect on client, server, or bidirectional
// streaming RPCs.
type UnaryInterceptorFunc func(UnaryCall) UnaryCall

// WrapUnary implements Interceptor by wrapping UnaryStream.Call.
func (f UnaryInterceptorFunc) WrapUnary(next UnaryStream) UnaryStream {
	return &wrappedUnaryStream{
		wrap:   f,
		stream: next,
	}
}

// WrapStreamContext implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapStreamContext(ctx context.Context) context.Context {
	return ctx
}

// WrapStreamSender implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapStreamSender(_ context.Context, sender Sender) Sender {
	return sender
}

// WrapStreamReceiver implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapStreamReceiver(_ context.Context, receiver Receiver) Receiver {
	return receiver
}

type wrappedUnaryStream struct {
	wrap   UnaryInterceptorFunc
	stream UnaryStream
}

func (s *wrappedUnaryStream) Call(ctx context.Context, req AnyRequest) (AnyResponse, error) {
	return s.wrap(s.stream.Call)(ctx, req)
}

func (s *wrappedUnaryStream) Spec() Specification {
	return s.stream.Spec()
}

// A chain composes multiple interceptors into one.
type chain struct {
	interceptors []Interceptor
}

var _ Interceptor = (*chain)(nil)

// newChain composes multiple interceptors into one.
func newChain(interceptors []Interceptor) *chain {
	// We usually wrap in reverse order to have the first interceptor from
	// the slice act first. Rather than doing this dance repeatedly, reverse the
	// interceptor order now.
	var chain chain
	for i := len(interceptors) - 1; i >= 0; i-- {
		if interceptor := interceptors[i]; interceptor != nil {
			chain.interceptors = append(chain.interceptors, interceptor)
		}
	}
	return &chain
}

func (c *chain) WrapUnary(next UnaryStream) UnaryStream {
	for _, interceptor := range c.interceptors {
		next = interceptor.WrapUnary(next)
	}
	return next
}

func (c *chain) WrapStreamContext(ctx context.Context) context.Context {
	for _, interceptor := range c.interceptors {
		ctx = interceptor.WrapStreamContext(ctx)
	}
	return ctx
}

func (c *chain) WrapStreamSender(ctx context.Context, sender Sender) Sender {
	if sender.Spec().IsClient {
		for _, interceptor := range c.interceptors {
			sender = interceptor.WrapStreamSender(ctx, sender)
		}
		return sender
	}
	// When we're wrapping senders on the handler side, we need to wrap in the
	// opposite order. See TestOnionOrderingEndToEnd.
	for i := len(c.interceptors) - 1; i >= 0; i-- {
		if interceptor := c.interceptors[i]; interceptor != nil {
			sender = interceptor.WrapStreamSender(ctx, sender)
		}
	}
	return sender
}

func (c *chain) WrapStreamReceiver(ctx context.Context, receiver Receiver) Receiver {
	for _, interceptor := range c.interceptors {
		receiver = interceptor.WrapStreamReceiver(ctx, receiver)
	}
	return receiver
}
