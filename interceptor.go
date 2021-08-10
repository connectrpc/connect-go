package rerpc

import (
	"context"
	"math"
	"time"

	"google.golang.org/protobuf/proto"
)

// Func is the generic signature of an RPC, from both the server and the
// client's perspective. Interceptors wrap Funcs.
type Func func(context.Context, proto.Message) (proto.Message, error)

// An Interceptor adds logic to a generated handler or client, like the
// decorators or middleware you may have seen in other libraries. Interceptors
// may replace the context, mutate the request, mutate the response, handle the
// returned error, retry, recover from panics, emit logs and metrics, or do
// nearly anything else.
//
// The returned Func must be safe to call concurrently. If chained carelessly,
// the interceptor's logic may run more than once - where possible,
// interceptors should be idempotent.
//
// See Chain for an example of interceptor use.
type Interceptor interface {
	Wrap(Func) Func
}

// ConfiguredCallInterceptor returns the Interceptor configured by a collection
// of call options (if any). It's used in generated code.
func ConfiguredCallInterceptor(opts ...CallOption) Interceptor {
	var cfg callCfg
	for _, o := range opts {
		o.applyToCall(&cfg)
	}
	return cfg.Interceptor
}

// ConfiguredHandlerInterceptor returns the Interceptor configured by a collection
// of handler options (if any). It's used in generated code.
func ConfiguredHandlerInterceptor(opts ...HandlerOption) Interceptor {
	var cfg handlerCfg
	for _, o := range opts {
		o.applyToHandler(&cfg)
	}
	return cfg.Interceptor
}

// ShortCircuit builds an interceptor that doesn't call the wrapped Func.
// Instead, it returns the supplied Error immediately.
//
// This is primarily useful when testing error handling. It's also used
// throughout reRPC's examples to avoid making network requests.
func ShortCircuit(err error) Interceptor {
	return InterceptorFunc(func(next Func) Func {
		return Func(func(_ context.Context, _ proto.Message) (proto.Message, error) {
			return nil, err
		})
	})
}

// An InterceptorFunc is a simple Interceptor implementation. See CallMetadata
// for an example.
type InterceptorFunc func(Func) Func

// Wrap implements Interceptor.
func (f InterceptorFunc) Wrap(next Func) Func { return f(next) }

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

type timeoutClamp struct {
	min, max time.Duration
}

var _ Interceptor = (*timeoutClamp)(nil)

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

func (c *timeoutClamp) Wrap(next Func) Func {
	return Func(func(ctx context.Context, req proto.Message) (proto.Message, error) {
		untilDeadline := time.Duration(math.MaxInt64)
		if deadline, ok := ctx.Deadline(); ok {
			untilDeadline = time.Until(deadline)
		}
		if c.max > 0 && untilDeadline > c.max {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.max)
			defer cancel()
		}
		if c.min > 0 && untilDeadline < c.min {
			return nil, errorf(CodeDeadlineExceeded, "timeout is %v, configured min is %v", untilDeadline, c.min)
		}
		return next(ctx, req)
	})
}

type recovery struct {
	Log func(context.Context, interface{})
}

var _ Interceptor = (*recovery)(nil)

// Recover wraps clients and handlers to recover from panics. It uses the
// supplied function to log the recovered value. The log function must be
// non-nil and safe to call concurrently. Keep in mind that panics initiated in
// other goroutines will still crash the process!
//
// When composed with other Interceptors in a Chain, Recover should be the
// outermost Interceptor.
func Recover(log func(context.Context, interface{})) Interceptor {
	return &recovery{log}
}

func (r *recovery) Wrap(next Func) Func {
	return Func(func(ctx context.Context, req proto.Message) (proto.Message, error) {
		defer r.recoverAndLog(ctx)
		return next(ctx, req)
	})
}

func (r *recovery) recoverAndLog(ctx context.Context) {
	if val := recover(); val != nil {
		r.Log(ctx, val)
	}
}
