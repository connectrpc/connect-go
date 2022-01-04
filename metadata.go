package rerpc

import (
	"context"
	"net/http"
)

type ctxk int

const (
	callMetaKey ctxk = iota
	handlerMetaKey
)

// Specification is a description of a client call or a handler invocation.
//
// Note that the Method, Service, and Package are protobuf names, not Go import
// paths or identifiers.
type Specification struct {
	Type    StreamType
	Package string // protobuf name, e.g. "acme.foo.v1"
	Service string // protobuf name, e.g. "FooService"
	Method  string // protobuf name, e.g. "Bar"

	Path                string
	ContentType         string
	RequestCompression  string
	ResponseCompression string
}

// Metadata provides a Specification and access to request and response headers
// for an in-progress client call or handler invocation.
type Metadata struct {
	Spec Specification
	req  *Header
	res  *Header
}

// Request returns the request headers.
func (m Metadata) Request() Header {
	if m.req == nil {
		return Header{raw: make(http.Header)}
	}
	return *m.req
}

// Response returns the response headers. In client-side Interceptors, the
// response isn't populated until the request is sent to the server.
func (m Metadata) Response() Header {
	if m.res == nil {
		return Header{raw: make(http.Header)}
	}
	return *m.res
}

// NewCallContext constructs a Metadata and attaches it to the supplied
// context. It's useful in tests that rely on CallMetadata.
func NewCallContext(ctx context.Context, spec Specification, req, res http.Header) context.Context {
	md := Metadata{
		Spec: spec,
		req:  &Header{raw: req},
		res:  &Header{raw: res},
	}
	return context.WithValue(ctx, callMetaKey, md)
}

// CallMetadata retrieves Metadata from the supplied context. It only succeeds
// in client calls - in other settings, the returned bool will be false. If
// you're writing an Interceptor that uses different logic for servers and
// clients, you can use CallMetadata to check which logic to apply.
//
// To test interceptors that use CallMetadata, pass them a context constructed
// by NewCallContext.
func CallMetadata(ctx context.Context) (Metadata, bool) {
	iface := ctx.Value(callMetaKey)
	if iface == nil {
		return Metadata{}, false
	}
	md, ok := iface.(Metadata)
	return md, ok
}

// NewHandlerContext constructs a HandlerMetadata and attaches it to the supplied
// context. It's useful in tests that call HandlerMeta.
func NewHandlerContext(ctx context.Context, spec Specification, req, res http.Header) context.Context {
	md := Metadata{
		Spec: spec,
		req:  &Header{raw: req},
		res:  &Header{raw: res},
	}
	return context.WithValue(ctx, handlerMetaKey, md)
}

// HandlerMetadata retrieves Metadata from the supplied context. It only
// succeeds in handler invocations (including protobuf service implementations)
// - in other settings, the returned bool will be false. If you're writing an
// Interceptor that uses different logic for servers and clients, you can use
// HandlerMetadata to check which logic to apply.
//
// To test interceptors and service implementations that use HandlerMetadata, pass
// them a context constructed by NewHandlerContext.
func HandlerMetadata(ctx context.Context) (Metadata, bool) {
	iface := ctx.Value(handlerMetaKey)
	if iface == nil {
		return Metadata{}, false
	}
	md, ok := iface.(Metadata)
	return md, ok
}

// WithoutMetadata strips any Metadata from the context.
func WithoutMetadata(ctx context.Context) context.Context {
	noCall := context.WithValue(ctx, callMetaKey, nil)
	return context.WithValue(noCall, handlerMetaKey, nil)
}
