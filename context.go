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

type CallInfo interface {
	// Spec returns a description of this call.
	Spec() Spec
	// Peer describes the other party for this call.
	Peer() Peer
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
	// RequestHeader returns the HTTP headers for this request. Headers beginning with
	// "Connect-" and "Grpc-" are reserved for use by the Connect and gRPC
	// protocols: applications may read them but shouldn't write them.
	RequestHeader() http.Header
	// ResponseHeader returns the HTTP headers for this response. Headers beginning with
	// "Connect-" and "Grpc-" are reserved for use by the Connect and gRPC
	// protocols: applications may read them but shouldn't write them.
	ResponseHeader() http.Header
	// ResponseTrailer returns the trailers for this response. Depending on the underlying
	// RPC protocol, trailers may be sent as HTTP trailers or a protocol-specific
	// block of in-body metadata.
	//
	// Trailers beginning with "Connect-" and "Grpc-" are reserved for use by the
	// Connect and gRPC protocols: applications may read them but shouldn't write
	// them.
	ResponseTrailer() http.Header

	internalOnly()
}

type callInfo struct {
	spec            Spec
	peer            Peer
	method          string
	requestHeader   http.Header
	responseHeader  http.Header
	responseTrailer http.Header
}

func (c *callInfo) Spec() Spec {
	return c.spec
}

func (c *callInfo) Peer() Peer {
	return c.peer
}

func (c *callInfo) RequestHeader() http.Header {
	if c.requestHeader == nil {
		c.requestHeader = make(http.Header)
	}
	return c.requestHeader
}

func (c *callInfo) ResponseHeader() http.Header {
	if c.responseHeader == nil {
		c.responseHeader = make(http.Header)
	}
	return c.responseHeader
}

func (c *callInfo) ResponseTrailer() http.Header {
	if c.responseTrailer == nil {
		c.responseTrailer = make(http.Header)
	}
	return c.responseTrailer
}

func (c *callInfo) HTTPMethod() string {
	return c.method
}

// internalOnly implements CallInfo.
func (c *callInfo) internalOnly() {}

type callInfoContextKey struct{}

// Create a new request context for use from a client. When the returned
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
	info, ok := ctx.Value(callInfoContextKey{}).(CallInfo)
	if !ok {
		info = &callInfo{}
		return context.WithValue(ctx, callInfoContextKey{}, info), info
	}
	return ctx, info
}

// CallInfoFromContext returns the CallInfo for the given context, if there is one.
func CallInfoFromContext(ctx context.Context) (CallInfo, bool) {
	value, ok := ctx.Value(callInfoContextKey{}).(CallInfo)
	return value, ok
}

func requestFromContext[T any](ctx context.Context, message *T) *Request[T] {
	request := NewRequest(message)
	callInfo, ok := CallInfoFromContext(ctx)
	if ok {
		request.setHeader(callInfo.RequestHeader())
	}
	return request
}
