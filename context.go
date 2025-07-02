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

package connect

import (
	"context"
	"net/http"
)

// CallInfo represents information relevant to an RPC call.
// Values returned by these methods are not thread-safe. Users should expect
// data races if they create an outgoing CallInfo in context and then pass that
// CallInfo to another goroutine and try to call methods on it concurrent with the RPC.
type CallInfo interface {
	// Spec returns a description of this call.
	Spec() Spec
	// Peer describes the other party for this call.
	Peer() Peer
	// RequestHeader returns the HTTP headers for this request. Headers beginning with
	// "Connect-" and "Grpc-" are reserved for use by the Connect and gRPC
	// protocols: applications may read them but shouldn't write them.
	RequestHeader() http.Header
	// ResponseHeader returns the HTTP headers for this response. Headers beginning with
	// "Connect-" and "Grpc-" are reserved for use by the Connect and gRPC
	// protocols: applications may read them but shouldn't write them.
	// On the client side, this method returns nil before
	// the call is actually made. After the call is made, for streaming operations,
	// this method will block for the server to actually return response headers.
	ResponseHeader() http.Header
	// ResponseTrailer returns the trailers for this response. Depending on the underlying
	// RPC protocol, trailers may be sent as HTTP trailers or a protocol-specific
	// block of in-body metadata.
	//
	// Trailers beginning with "Connect-" and "Grpc-" are reserved for use by the
	// Connect and gRPC protocols: applications may read them but shouldn't write
	// them.
	//
	// On the client side, this method returns nil before
	// the call is actually made. After the call is made, for streaming operations,
	// this method will block for the server to actually return response trailers.
	ResponseTrailer() http.Header
	// HTTPMethod returns the HTTP method for this request. This is nearly always
	// POST, but side-effect-free unary RPCs could be made via a GET.
	//
	// On a newly created request, via NewRequest, this will return the empty
	// string until the actual request is actually sent and the HTTP method
	// determined. This means that client interceptor functions will see the
	// empty string until *after* they delegate to the handler they wrapped. It
	// is even possible for this to return the empty string after such delegation,
	// if the request was never actually sent to the server (and thus no
	// determination ever made about the HTTP method).
	HTTPMethod() string

	internalOnly()
}

// Create a new outgoing context for use from a client. When the returned
// context is passed to RPCs, the returned call info can be used to set
// request metadata before the RPC is invoked and to inspect response
// metadata after the RPC completes.
//
// The returned context may be re-used across RPCs as long as they are
// not concurrent. Results of all CallInfo methods other than
// RequestHeader() are undefined if the context is used with concurrent RPCs.
// If the given context is already associated with an outgoing CallInfo, then
// ctx and the existing CallInfo are returned.
func NewOutgoingContext(ctx context.Context) (context.Context, CallInfo) {
	return newOutgoingContext(ctx)
}

// CallInfoFromOutgoingContext returns the CallInfo for the given outgoing context, if there is one.
func CallInfoFromOutgoingContext(ctx context.Context) (CallInfo, bool) {
	value, ok := ctx.Value(outgoingCallInfoContextKey{}).(CallInfo)
	return value, ok
}

// CallInfoFromIncomingContext returns the CallInfo for the given incoming context, if there is one.
func CallInfoFromIncomingContext(ctx context.Context) (CallInfo, bool) {
	value, ok := ctx.Value(incomingCallInfoContextKey{}).(CallInfo)
	return value, ok
}

// handlerCallInfo is a CallInfo implementation used for handlers.
type handlerCallInfo struct {
	spec            Spec
	peer            Peer
	method          string
	requestHeader   http.Header
	responseHeader  http.Header
	responseTrailer http.Header
}

func (c *handlerCallInfo) Spec() Spec {
	return c.spec
}

func (c *handlerCallInfo) Peer() Peer {
	return c.peer
}

func (c *handlerCallInfo) RequestHeader() http.Header {
	if c.requestHeader == nil {
		c.requestHeader = make(http.Header)
	}
	return c.requestHeader
}

func (c *handlerCallInfo) ResponseHeader() http.Header {
	if c.responseHeader == nil {
		c.responseHeader = make(http.Header)
	}
	return c.responseHeader
}

func (c *handlerCallInfo) ResponseTrailer() http.Header {
	if c.responseTrailer == nil {
		c.responseTrailer = make(http.Header)
	}
	return c.responseTrailer
}

func (c *handlerCallInfo) HTTPMethod() string {
	return c.method
}

// internalOnly implements CallInfo.
func (c *handlerCallInfo) internalOnly() {}

// streamCallInfo is a CallInfo implementation used for streaming RPC handlers.
type streamCallInfo struct {
	conn StreamingHandlerConn
}

func (c *streamCallInfo) Spec() Spec {
	return c.conn.Spec()
}

func (c *streamCallInfo) Peer() Peer {
	return c.conn.Peer()
}

func (c *streamCallInfo) RequestHeader() http.Header {
	return c.conn.RequestHeader()
}

func (c *streamCallInfo) ResponseHeader() http.Header {
	return c.conn.ResponseHeader()
}

func (c *streamCallInfo) ResponseTrailer() http.Header {
	return c.conn.ResponseTrailer()
}

func (c *streamCallInfo) HTTPMethod() string {
	// All stream calls are POSTs
	return http.MethodPost
}

// internalOnly implements CallInfo.
func (c *streamCallInfo) internalOnly() {}

// clientCallInfo is a CallInfo implementation used for clients.
type clientCallInfo struct {
	responseSource
	spec          Spec
	peer          Peer
	method        string
	requestHeader http.Header
}

func (c *clientCallInfo) Spec() Spec {
	return c.spec
}

func (c *clientCallInfo) Peer() Peer {
	return c.peer
}

func (c *clientCallInfo) RequestHeader() http.Header {
	if c.requestHeader == nil {
		c.requestHeader = make(http.Header)
	}
	return c.requestHeader
}

func (c *clientCallInfo) ResponseHeader() http.Header {
	if c.responseSource == nil {
		return nil
	}
	return c.responseSource.ResponseHeader()
}

func (c *clientCallInfo) ResponseTrailer() http.Header {
	if c.responseSource == nil {
		return nil
	}
	return c.responseSource.ResponseTrailer()
}

func (c *clientCallInfo) HTTPMethod() string {
	return c.method
}

// internalOnly implements CallInfo.
func (c *clientCallInfo) internalOnly() {}

type outgoingCallInfoContextKey struct{}
type incomingCallInfoContextKey struct{}

// responseSource indicates a type that manage response headers and trailers.
type responseSource interface {
	ResponseHeader() http.Header
	ResponseTrailer() http.Header
}

// responseWrapper wraps a Response object so that it can implement the responseSource interface.
type responseWrapper[Res any] struct {
	response *Response[Res]
}

func (w *responseWrapper[Res]) ResponseHeader() http.Header {
	return w.response.Header()
}

func (w *responseWrapper[Res]) ResponseTrailer() http.Header {
	return w.response.Trailer()
}

// Creates a new outgoing context or returns the existing one in context.
func newOutgoingContext(ctx context.Context) (context.Context, *clientCallInfo) {
	info, ok := ctx.Value(outgoingCallInfoContextKey{}).(*clientCallInfo)
	if !ok {
		info = &clientCallInfo{}
		return context.WithValue(ctx, outgoingCallInfoContextKey{}, info), info
	}
	return ctx, info
}

// newOutgoingContext creates a new incoming context.
func newIncomingContext(ctx context.Context, info CallInfo) context.Context {
	return context.WithValue(ctx, incomingCallInfoContextKey{}, info)
}

// requestFromOutgoingContext creates a new Request using the given context and message.
func requestFromOutgoingContext[T any](ctx context.Context, message *T) *Request[T] {
	request := NewRequest(message)
	callInfo, ok := CallInfoFromOutgoingContext(ctx)
	if ok {
		request.setHeader(callInfo.RequestHeader())
	}
	return request
}
