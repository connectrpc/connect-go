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

package connect

import (
	"context"
	"net/http"

	connectv2 "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
)

// CallInfo represents information relevant to an RPC call.
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
	// On the client side, this method returns nil before the call is actually made.
	// After the call is made, for streaming operations, this method will block
	// for the server to actually return response trailers.
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

// NewClientContext creates a new client (i.e. outgoing) context for use from a
// client. When the returned context is passed to RPCs, the returned call info
// can be used to set request metadata before the RPC is invoked and to inspect
// response metadata after the RPC completes.
//
// The returned context may be re-used across RPCs as long as they are
// not concurrent. Results of all CallInfo methods other than
// RequestHeader() are undefined if the context is used with concurrent RPCs.
func NewClientContext(ctx context.Context) (context.Context, CallInfo) {
	info := &clientCallInfo{}
	ctx = context.WithValue(ctx, clientCallInfoContextKey{}, info)
	return ctx, info
}

// CallInfoForHandlerContext returns the CallInfo for the given handler (i.e.
// incoming) context, if there is one.
func CallInfoForHandlerContext(ctx context.Context) (CallInfo, bool) {
	value, ok := ctx.Value(handlerCallInfoContextKey{}).(CallInfo)
	return value, ok
}

// clientCallInfoContextKey is the key used to store client call info in context.
type clientCallInfoContextKey struct{}

// sentinelContextKey holds a snapshot of [clientCallInfoContextKey]'s value
// at dispatch time. Each interceptor thunk compares the live value against
// the snapshot; a divergence means an interceptor swapped in a freshly
// minted CallInfo via [NewClientContext], which would silently strip any
// chain-accumulated headers.
type sentinelContextKey struct{}

// installSentinel records the current CallInfo (creating one if necessary)
// under [sentinelContextKey] so [checkClientContextSentinel] can detect
// interceptors that introduce a new client context. It returns the augmented
// context.
func installSentinel(ctx context.Context) context.Context {
	if _, ok := clientCallInfoFromContext(ctx); !ok {
		ctx, _ = NewClientContext(ctx)
	}
	info, _ := clientCallInfoFromContext(ctx)
	return context.WithValue(ctx, sentinelContextKey{}, info)
}

// checkClientContextSentinel reports whether the CallInfo in ctx matches the
// one captured by [installSentinel]. A mismatch happens when an interceptor
// re-wrapped ctx with [NewClientContext]; v1 surfaced this as an explicit
// error rather than letting the chain silently lose state.
func checkClientContextSentinel(ctx context.Context) error {
	if ctx.Value(clientCallInfoContextKey{}) != ctx.Value(sentinelContextKey{}) {
		return errNewClientContextProhibited
	}
	return nil
}

// handlerCallInfoContextKey is the key used to store handler call info in context.
type handlerCallInfoContextKey struct{}

// clientCallInfo is the v1-facing CallInfo implementation for clients.
// It owns the request header and reads response/trailer state from the
// underlying v2 [*connectv2.CallInfo] populated by the transport.
type clientCallInfo struct {
	spec   Spec
	peer   Peer
	method string

	requestHeader http.Header

	// v2Info, if non-nil, is the v2 call info attached on dispatch. We
	// project its ResponseHeader and ResponseTrailer back into v1 headers
	// on demand so the v1 API contract (live access to response headers/
	// trailers) is preserved.
	v2Info *connectv2.CallInfo
	// httpInfo, if non-nil, is the connecthttp ClientInfo whose embedded
	// CallInfo matches v2Info. It is used to surface the HTTP-level
	// HTTPMethod once the transport has dispatched.
	httpInfo *connecthttp.ClientInfo
}

func (c *clientCallInfo) Spec() Spec { return c.spec }
func (c *clientCallInfo) Peer() Peer { return c.peer }
func (c *clientCallInfo) RequestHeader() http.Header {
	if c.requestHeader == nil {
		c.requestHeader = make(http.Header)
	}
	return c.requestHeader
}
func (c *clientCallInfo) ResponseHeader() http.Header {
	if c.v2Info == nil {
		return nil
	}
	return metadataToHTTPHeader(c.v2Info.ResponseHeader())
}
func (c *clientCallInfo) ResponseTrailer() http.Header {
	if c.v2Info == nil {
		return nil
	}
	return metadataToHTTPHeader(c.v2Info.ResponseTrailer())
}
func (c *clientCallInfo) HTTPMethod() string {
	if c.httpInfo != nil {
		if m := c.httpInfo.HTTPMethod(); m != "" {
			return m
		}
	}
	return c.method
}
func (c *clientCallInfo) internalOnly() {}

// handlerCallInfo is the v1-facing CallInfo for unary handlers. The request
// header is captured once from the v2 [*connectv2.CallInfo] at handler entry;
// response header and trailer are owned by the v1 handler so user code can
// mutate them freely before the shim copies them into v2 metadata on send.
type handlerCallInfo struct {
	spec            Spec
	peer            Peer
	method          string
	requestHeader   http.Header
	responseHeader  http.Header
	responseTrailer http.Header
}

func (c *handlerCallInfo) Spec() Spec { return c.spec }
func (c *handlerCallInfo) Peer() Peer { return c.peer }
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
	if c.method != "" {
		return c.method
	}
	return http.MethodPost
}
func (c *handlerCallInfo) internalOnly() {}

// streamingHandlerCallInfo is the v1-facing CallInfo for streaming handlers.
// It delegates all accessors to the underlying conn so user code observing the
// CallInfo sees the same state as user code holding the conn directly.
type streamingHandlerCallInfo struct {
	conn StreamingHandlerConn
}

func (c *streamingHandlerCallInfo) Spec() Spec                  { return c.conn.Spec() }
func (c *streamingHandlerCallInfo) Peer() Peer                  { return c.conn.Peer() }
func (c *streamingHandlerCallInfo) RequestHeader() http.Header  { return c.conn.RequestHeader() }
func (c *streamingHandlerCallInfo) ResponseHeader() http.Header { return c.conn.ResponseHeader() }
func (c *streamingHandlerCallInfo) ResponseTrailer() http.Header {
	return c.conn.ResponseTrailer()
}
func (c *streamingHandlerCallInfo) HTTPMethod() string { return http.MethodPost }
func (c *streamingHandlerCallInfo) internalOnly()      {}

// clientCallInfoFromContext returns the v1 client CallInfo from ctx if one
// is attached via [NewClientContext].
func clientCallInfoFromContext(ctx context.Context) (*clientCallInfo, bool) {
	v, ok := ctx.Value(clientCallInfoContextKey{}).(*clientCallInfo)
	return v, ok
}

// newHandlerContext returns ctx augmented with the given handler call info so
// [CallInfoForHandlerContext] can find it.
func newHandlerContext(ctx context.Context, info CallInfo) context.Context {
	return context.WithValue(ctx, handlerCallInfoContextKey{}, info)
}

// metadataToHTTPHeader returns a live http.Header view onto m's keys. Mutations
// to the returned header do not propagate back to m. Used wherever v1 code
// expects an http.Header.
func metadataToHTTPHeader(m *connectv2.Header) http.Header {
	if m == nil {
		return http.Header{}
	}
	h := make(http.Header, m.Len())
	for k, vs := range m.All() {
		h[k] = append([]string(nil), vs...)
	}
	return h
}

// copyHTTPHeaderIntoMetadata merges src into dst. Existing keys in dst are
// overwritten; keys in src that already exist in dst preserve dst's previous
// values for headers not in src. Used when the shim transfers the user's
// http.Header onto v2's Metadata at the call boundary.
func copyHTTPHeaderIntoMetadata(dst *connectv2.Header, src http.Header) {
	if dst == nil {
		return
	}
	for k, vs := range src {
		if len(vs) == 0 {
			continue
		}
		dst.SetValues(k, append([]string(nil), vs...))
	}
}

// mergeMetadataWithHTTPHeader appends every value in src into dst. Unlike
// [copyHTTPHeaderIntoMetadata], existing values in dst are preserved and src
// values are added with [connectv2.Header.Add], so calling this twice with
// different sources accumulates instead of overwriting.
func mergeMetadataWithHTTPHeader(dst *connectv2.Header, src http.Header) {
	if dst == nil {
		return
	}
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
