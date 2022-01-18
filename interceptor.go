package rerpc

import (
	"context"
)

type AnyRequest interface {
	Any() any
	Spec() Specification
	Header() Header

	// Only internal implementations, so we can add methods without breaking
	// backward compatibility.
	internalOnly()
}

type AnyResponse interface {
	Any() any
	Header() Header

	// Only internal implementations, so we can add methods without breaking
	// backward compatibility.
	internalOnly()
}

// Func is the generic signature of a unary RPC. Interceptors wrap Funcs.
//
// The type of the request and response struct depend on the codec being used.
// When using protobuf, they'll always be proto.Message implementations.
type Func func(context.Context, AnyRequest) (AnyResponse, error)

// StreamFunc is the generic signature of a streaming RPC. Interceptors wrap
// StreamFuncs.
type StreamFunc func(context.Context) (context.Context, Sender, Receiver)

// An Interceptor adds logic to a generated handler or client, like the
// decorators or middleware you may have seen in other libraries. Interceptors
// may replace the context, mutate the request, mutate the response, handle the
// returned error, retry, recover from panics, emit logs and metrics, or do
// nearly anything else.
//
// The functions returned by Wrap and WrapStream must be safe to call
// concurrently. If chained carelessly, the interceptor's logic may run more
// than once - where possible, interceptors should be idempotent.
//
// See Chain for an example of interceptor use.
type Interceptor interface {
	Wrap(Func) Func
	WrapStream(StreamFunc) StreamFunc
}

// A UnaryInterceptorFunc is a simple Interceptor implementation that only
// wraps unary RPCs. It has no effect on client, server, or bidirectional
// streaming RPCs. See CallMetadata for an example.
type UnaryInterceptorFunc func(Func) Func

// Wrap implements Interceptor by applying the interceptor function.
func (f UnaryInterceptorFunc) Wrap(next Func) Func { return f(next) }

// WrapStream implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapStream(next StreamFunc) StreamFunc {
	return next
}

// A Chain composes multiple interceptors into one.
type Chain struct {
	interceptors []Interceptor
}

var _ Interceptor = (*Chain)(nil)

// NewChain composes multiple interceptors into one. The first interceptor
// provided is the outermost layer of the onion: it acts first on the context
// and request, and last on the response and error.
func NewChain(interceptors ...Interceptor) *Chain {
	return &Chain{interceptors}
}

// Wrap implements Interceptor.
func (c *Chain) Wrap(next Func) Func {
	// We need to wrap in reverse order to have the first interceptor from
	// the slice act first.
	for i := len(c.interceptors) - 1; i >= 0; i-- {
		if interceptor := c.interceptors[i]; interceptor != nil {
			next = interceptor.Wrap(next)
		}
	}
	return next
}

// WrapStream implements Interceptor.
func (c *Chain) WrapStream(next StreamFunc) StreamFunc {
	// We need to wrap in reverse order to have the first interceptor from
	// the slice act first.
	for i := len(c.interceptors) - 1; i >= 0; i-- {
		if interceptor := c.interceptors[i]; interceptor != nil {
			next = interceptor.WrapStream(next)
		}
	}
	return next
}
