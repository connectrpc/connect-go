package connect_test

import (
	"context"

	"github.com/bufconnect/connect"
)

type shortCircuit struct {
	err error
}

var _ connect.Interceptor = (*shortCircuit)(nil)

func (sc *shortCircuit) Wrap(next connect.Func) connect.Func {
	return connect.Func(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, sc.err
	})
}

func (sc *shortCircuit) WrapContext(ctx context.Context) context.Context {
	return ctx
}

func (sc *shortCircuit) WrapSender(_ context.Context, s connect.Sender) connect.Sender {
	return &errSender{Sender: s, err: sc.err}
}

func (sc *shortCircuit) WrapReceiver(_ context.Context, r connect.Receiver) connect.Receiver {
	return &errReceiver{Receiver: r, err: sc.err}
}

// ShortCircuit builds an interceptor that doesn't call the wrapped RPC at all.
// Instead, it returns the supplied Error immediately. ShortCircuit works for
// unary and streaming RPCs.
//
// This is primarily useful when testing error handling. It's also used
// throughout connect's examples to avoid making network requests.
func ShortCircuit(err error) connect.Interceptor {
	return &shortCircuit{err}
}

type errSender struct {
	connect.Sender

	err error
}

var _ connect.Sender = (*errSender)(nil)

func (s *errSender) Send(_ any) error    { return s.err }
func (s *errSender) Close(_ error) error { return s.err }

type errReceiver struct {
	connect.Receiver

	err error
}

var _ connect.Receiver = (*errReceiver)(nil)

func (r *errReceiver) Receive(_ any) error { return r.err }
func (r *errReceiver) Close() error        { return r.err }
