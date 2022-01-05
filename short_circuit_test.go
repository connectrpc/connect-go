package rerpc_test

import (
	"context"

	"github.com/rerpc/rerpc"
)

type shortCircuit struct {
	err error
}

var _ rerpc.Interceptor = (*shortCircuit)(nil)

func (sc *shortCircuit) Wrap(next rerpc.Func) rerpc.Func {
	return rerpc.Func(func(_ context.Context, _ rerpc.AnyRequest) (rerpc.AnyResponse, error) {
		return nil, sc.err
	})
}

func (sc *shortCircuit) WrapStream(next rerpc.StreamFunc) rerpc.StreamFunc {
	return rerpc.StreamFunc(func(ctx context.Context) (context.Context, rerpc.Stream) {
		ctx, stream := next(ctx)
		return ctx, &errStream{Stream: stream, err: sc.err}
	})
}

// ShortCircuit builds an interceptor that doesn't call the wrapped RPC at all.
// Instead, it returns the supplied Error immediately. ShortCircuit works for
// unary and streaming RPCs.
//
// This is primarily useful when testing error handling. It's also used
// throughout reRPC's examples to avoid making network requests.
func ShortCircuit(err error) rerpc.Interceptor {
	return &shortCircuit{err}
}

type errStream struct {
	rerpc.Stream

	err error
}

var _ rerpc.Stream = (*errStream)(nil)

func (s *errStream) Receive(_ interface{}) error { return s.err }
func (s *errStream) CloseReceive() error         { return s.err }
func (s *errStream) Send(_ interface{}) error    { return s.err }
func (s *errStream) CloseSend(_ error) error     { return s.err }
