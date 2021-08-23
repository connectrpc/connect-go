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
	ReadMaxBytes        int64
}

// CallMetadata provides a Specification and access to request and response
// headers for an in-progress client call. It's useful in Interceptors.
type CallMetadata struct {
	Spec Specification
	req  *Header
	res  *Header
}

// Request returns the request headers.
func (m CallMetadata) Request() Header {
	if m.req == nil {
		return NewHeader(make(http.Header))
	}
	return *m.req
}

// Response returns the response headers. In Interceptors, the response isn't
// populated until the request is sent to the server.
func (m CallMetadata) Response() Header {
	if m.res == nil {
		return NewHeader(make(http.Header))
	}
	return *m.res
}

// NewCallContext constructs a CallMetadata and attaches it to the supplied
// context. It's useful in tests that rely on CallMeta.
func NewCallContext(ctx context.Context, spec Specification, req, res http.Header) context.Context {
	md := CallMetadata{
		Spec: spec,
		req:  &Header{raw: req},
		res:  &Header{raw: res},
	}
	return context.WithValue(ctx, callMetaKey, md)
}

// CallMeta retrieves CallMetadata from the supplied context. It only succeeds
// in client calls - in other settings, the returned bool will be false. If
// you're writing an Interceptor that uses different logic for servers and
// clients, you can use CallMeta to check which logic to apply.
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
// headers for an in-progress handler invocation. It's useful in Interceptors
// and protobuf service implementations.
type HandlerMetadata struct {
	Spec Specification
	req  *Header
	res  *Header
}

// Request returns the request headers.
func (hm HandlerMetadata) Request() Header {
	if hm.req == nil {
		return NewHeader(make(http.Header))
	}
	return *hm.req
}

// Response returns the response headers.
func (hm HandlerMetadata) Response() Header {
	if hm.res == nil {
		return NewHeader(make(http.Header))
	}
	return *hm.res
}

// NewHandlerContext constructs a HandlerMetadata and attaches it to the supplied
// context. It's useful in tests that call HandlerMeta.
func NewHandlerContext(ctx context.Context, spec Specification, req, res http.Header) context.Context {
	md := HandlerMetadata{
		Spec: spec,
		req:  &Header{raw: req},
		res:  &Header{raw: res},
	}
	return context.WithValue(ctx, handlerMetaKey, md)
}

// HandlerMeta retrieves HandlerMetadata from the supplied context. It only
// succeeds in handler invocations (including protobuf service implementations)
// - in other settings, the returned bool will be false. If you're writing an
// Interceptor that uses different logic for servers and clients, you can use
// HandlerMeta to check which logic to apply.
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

// WithoutMeta strips any CallMetadata and HandlerMetadata from the context.
func WithoutMeta(ctx context.Context) context.Context {
	noCall := context.WithValue(ctx, callMetaKey, nil)
	return context.WithValue(noCall, handlerMetaKey, nil)
}
