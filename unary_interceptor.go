package connect

import "context"

// A UnaryInterceptorFunc is a simple Interceptor implementation that only
// wraps unary RPCs. It has no effect on client, server, or bidirectional
// streaming RPCs. See CallMetadata for an example.
type UnaryInterceptorFunc func(Func) Func

// Wrap implements Interceptor by applying the interceptor function.
func (f UnaryInterceptorFunc) Wrap(next Func) Func { return f(next) }

// WrapContext implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapContext(ctx context.Context) context.Context {
	return ctx
}

// WrapSender implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapSender(_ context.Context, s Sender) Sender {
	return s
}

// WrapReceiver implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapReceiver(_ context.Context, r Receiver) Receiver {
	return r
}
