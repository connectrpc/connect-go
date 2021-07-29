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
// Note that the Method, Service, and Package are fully-qualified protobuf
// names, not Go import paths or identifiers.
type Specification struct {
	Method  string // full protobuf name, e.g. "acme.foo.v1.Foo.Bar"
	Service string // full protobuf name, e.g. "acme.foo.v1.Foo"
	Package string // full protobuf name, e.g. "acme.foo.v1"

	Path                string
	ContentType         string
	RequestCompression  string
	ResponseCompression string
}

// CallMetadata provides a Specification and access to request and response
// headers for an in-progress client call. It's useful in CallInterceptors.
type CallMetadata struct {
	Spec Specification
	req  *MutableHeader
	res  *ImmutableHeader
}

// Request returns a writable view of the request headers.
func (m CallMetadata) Request() MutableHeader {
	if m.req == nil {
		return NewMutableHeader(make(http.Header))
	}
	return *m.req
}

// Response returns a read-only view of the response headers. In
// CallInterceptors, the response isn't populated until the request is sent to
// the server.
func (m CallMetadata) Response() ImmutableHeader {
	if m.res == nil {
		return NewImmutableHeader(nil) // nil maps are safe to read from
	}
	return *m.res
}

// NewCallContext constructs a CallMetadata and attaches it to the supplied
// context. It's useful in tests that call CallMeta.
func NewCallContext(ctx context.Context, spec Specification, req, res http.Header) context.Context {
	mutable := NewMutableHeader(req)
	immutable := NewImmutableHeader(res)
	md := CallMetadata{
		Spec: spec,
		req:  &mutable,
		res:  &immutable,
	}
	return context.WithValue(ctx, callMetaKey, md)
}

// CallMeta retrieves CallMetadata from the supplied context. It only succeeds
// in CallInterceptors - in other settings, the returned bool will be false.
//
// To test interceptors that use CallMeta, pass them a context constructed by
// NewCallContext.
func CallMeta(ctx context.Context) (CallMetadata, bool) {
	iface := ctx.Value(callMetaKey)
	if iface == nil {
		return CallMetadata{}, false
	}
	md, ok := iface.(CallMetadata)
	return md, ok
}

// HandlerMetadata provides a Specification and access to request and response
// headers for an in-progress handler invocation. It's useful in
// HandlerInterceptors and protobuf service implementations.
type HandlerMetadata struct {
	Spec Specification
	req  *ImmutableHeader
	res  *MutableHeader
}

// Request returns a read-only view of the request headers.
func (hm HandlerMetadata) Request() ImmutableHeader {
	if hm.req == nil {
		return NewImmutableHeader(nil) // nil maps are safe to read from
	}
	return *hm.req
}

// Response returns a writable view of the response headers.
func (hm HandlerMetadata) Response() MutableHeader {
	if hm.res == nil {
		return NewMutableHeader(make(http.Header))
	}
	return *hm.res
}

// NewHandlerContext constructs a HandlerMetadata and attaches it to the supplied
// context. It's useful in tests that call HandlerMeta.
func NewHandlerContext(ctx context.Context, spec Specification, req, res http.Header) context.Context {
	immutable := NewImmutableHeader(req)
	mutable := NewMutableHeader(res)
	md := HandlerMetadata{
		Spec: spec,
		req:  &immutable,
		res:  &mutable,
	}
	return context.WithValue(ctx, handlerMetaKey, md)
}

// HandlerMeta retrieves HandlerMetadata from the supplied context. It only
// succeeds in HandlerInterceptors and protobuf service implementations - in
// other settings, the returned bool will be false.
//
// To test interceptors and service implementations that use HandlerMeta, pass
// them a context constructed by NewHandlerContext.
func HandlerMeta(ctx context.Context) (HandlerMetadata, bool) {
	iface := ctx.Value(handlerMetaKey)
	if iface == nil {
		return HandlerMetadata{}, false
	}
	md, ok := iface.(HandlerMetadata)
	return md, ok
}
