package connect

import "context"

// A UnaryInterceptorFunc is a simple Interceptor implementation that only
// wraps unary RPCs. It has no effect on client, server, or bidirectional
// streaming RPCs.
type UnaryInterceptorFunc func(Func) Func

// WrapUnary implements Interceptor by applying the interceptor function.
func (f UnaryInterceptorFunc) WrapUnary(next Func) Func { return f(next) }

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
