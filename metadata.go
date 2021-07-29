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
type Specification struct {
	Method string
	// TODO: service and package too?
	ContentType         string
	RequestCompression  string
	ResponseCompression string
}

// CallMetadata provides a Specification and access to request and response
// headers for an in-progress client call. It's useful in CallInterceptors.
type CallMetadata struct {
	Spec     Specification
	Request  *MutableHeader
	Response *ImmutableHeader
}

// NewCallContext constructs a CallMetadata and attaches it to the supplied
// context. It's useful in tests that call CallMeta.
func NewCallContext(ctx context.Context, spec Specification, req, res http.Header) context.Context {
	md := CallMetadata{
		Spec:     spec,
		Request:  NewMutableHeader(req),
		Response: NewImmutableHeader(res),
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
	Spec     Specification
	Request  *ImmutableHeader
	Response *MutableHeader
}

// NewHandlerContext constructs a HandlerMetadata and attaches it to the supplied
// context. It's useful in tests that call HandlerMeta.
func NewHandlerContext(ctx context.Context, spec Specification, req, res http.Header) context.Context {
	md := HandlerMetadata{
		Spec:     spec,
		Request:  NewImmutableHeader(req),
		Response: NewMutableHeader(res),
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
