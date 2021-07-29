package rerpc

import (
	"context"
	"math"
	"time"

	"google.golang.org/protobuf/proto"
)

// UnaryCall is the generic signature of all client methods. Client-side
// interceptors wrap UnaryCalls.
type UnaryCall func(ctx context.Context, request, response proto.Message) error

// UnaryHandler is the generic signature of all handlers. Server-side
// interceptors wrap UnaryHandlers.
type UnaryHandler func(context.Context, proto.Message) (proto.Message, error)

// A CallInterceptor adds logic to a generated client, like the decorators or
// middleware you may have seen in other libraries. Interceptors may replace
// the context, mutate the request, mutate the response, handle the returned
// error, retry, or recover from panics.
//
// The returned UnaryCall must be safe to call concurrently. If chained
// carelessly, the interceptor's logic may run more than once on each client
// call - where possible, interceptors should be idempotent.
type CallInterceptor interface {
	WrapCall(UnaryCall) UnaryCall
}

// A HandlerInterceptor adds logic to a generated handler, like the decorators
// or middleware you may have seen in other libraries. Interceptors may replace
// the context, mutate the request, mutate the response, handle the returned
// error, retry, or recover from panics.
//
// The returned UnaryHandler must return either a non-nil response or a non-nil
// error (or both), and it must be safe to call concurrently. If chained
// carelessly, the interceptor's logic may run more than once on each request -
// where possible, interceptors should be idempotent.
type HandlerInterceptor interface {
	WrapHandler(UnaryHandler) UnaryHandler
}

// An Interceptor is both a CallInterceptor and a HandlerInterceptor.
type Interceptor interface {
	CallInterceptor
	HandlerInterceptor
}

var (
	_ Interceptor   = (*Chain)(nil)
	_ CallOption    = (*Chain)(nil)
	_ HandlerOption = (*Chain)(nil)
)

// A Chain composes multiple interceptors into one. A Chain is an Interceptor,
// a CallOption, and a HandlerOption. Note that when used as a CallOption or a
// HandlerOption, a Chain replaces any previously-configured interceptors.
type Chain struct {
	interceptors []Interceptor
}

// NewChain composes multiple interceptors into one. The first interceptor
// provided is the outermost layer of the onion: it acts first on the context
// and request, and last on the response and error.
func NewChain(interceptors ...Interceptor) *Chain {
	return &Chain{interceptors}
}

// WrapCall implements CallInterceptor.
func (c *Chain) WrapCall(next UnaryCall) UnaryCall {
	// We need to wrap in reverse order to have the first interceptor from
	// the slice act first.
	for i := len(c.interceptors) - 1; i >= 0; i-- {
		if interceptor := c.interceptors[i]; interceptor != nil {
			next = interceptor.WrapCall(next)
		}
	}
	return next
}

// WrapHandler implements HandlerInterceptor.
func (c *Chain) WrapHandler(next UnaryHandler) UnaryHandler {
	// We need to wrap in reverse order to have the first interceptor from
	// the slice act first.
	for i := len(c.interceptors) - 1; i >= 0; i-- {
		if interceptor := c.interceptors[i]; interceptor != nil {
			next = interceptor.WrapHandler(next)
		}
	}
	return next
}

func (c *Chain) applyToCall(cfg *callCfg) {
	cfg.Interceptor = c
}

func (c *Chain) applyToHandler(cfg *handlerCfg) {
	cfg.Interceptor = c
}

type timeoutClamp struct {
	min, max time.Duration
}

// ClampTimeout sets the minimum and maximum allowable timeouts for clients and
// handlers.
//
// For both clients and handlers, calls with less than the minimum timeout
// return CodeDeadlineExceeded. In that case, clients don't send any data to
// the server. Setting min to zero disables this behavior (though clients and
// handlers always return CodeDeadlineExceeded if the deadline has already
// passed).
//
// For both clients and handlers, setting the max timeout to a positive value
// caps the allowed timeout. Calls with a timeout larger than the max, or calls
// with no timeout at all, have their timeouts reduced to the maximum allowed
// value.
func ClampTimeout(min, max time.Duration) Interceptor {
	return &timeoutClamp{min, max}
}

func (c *timeoutClamp) WrapCall(next UnaryCall) UnaryCall {
	return UnaryCall(func(ctx context.Context, req proto.Message, res proto.Message) error {
		ctx, cancel, err := c.clamp(ctx)
		if err != nil {
			return err
		}
		defer cancel()
		return next(ctx, req, res)
	})
}

func (c *timeoutClamp) WrapHandler(next UnaryHandler) UnaryHandler {
	return UnaryHandler(func(ctx context.Context, req proto.Message) (proto.Message, error) {
		ctx, cancel, err := c.clamp(ctx)
		if err != nil {
			return nil, err
		}
		defer cancel()
		return next(ctx, req)
	})
}

func (c *timeoutClamp) clamp(ctx context.Context) (context.Context, context.CancelFunc, error) {
	untilDeadline := time.Duration(math.MaxInt64)
	if deadline, ok := ctx.Deadline(); ok {
		untilDeadline = time.Until(deadline)
	}
	if c.max > 0 && untilDeadline > c.max {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.max)
		return ctx, cancel, nil
	}
	if c.min > 0 && untilDeadline < c.min {
		return nil, nil, errorf(CodeDeadlineExceeded, "timeout is %v, configured min is %v", untilDeadline, c.min)
	}
	return ctx, context.CancelFunc(func() {}), nil
}

type recovery struct {
	Log func(context.Context, interface{})
}

// Recover wraps clients and handlers to recover from panics. It uses the
// supplied function to log the recovered value. The log function must be
// non-nil and safe to call concurrently.
//
// Keep in mind that this will not prevent all crashes! Panics initiated in
// other goroutines will still crash the process.
//
// When composed with other Interceptors in a Chain, Recover should be the
// outermost Interceptor.
func Recover(log func(context.Context, interface{})) Interceptor {
	return &recovery{log}
}

func (r *recovery) WrapCall(next UnaryCall) UnaryCall {
	return UnaryCall(func(ctx context.Context, req proto.Message, res proto.Message) error {
		defer r.recoverAndLog(ctx)
		return next(ctx, req, res)
	})
}

func (r *recovery) WrapHandler(next UnaryHandler) UnaryHandler {
	return UnaryHandler(func(ctx context.Context, req proto.Message) (proto.Message, error) {
		defer r.recoverAndLog(ctx)
		return next(ctx, req)
	})
}

func (r *recovery) recoverAndLog(ctx context.Context) {
	if val := recover(); val != nil {
		r.Log(ctx, val)
	}
}
