package rerpc

import (
	"bytes"
	"context"
	"io"
	"testing"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/rerpc/rerpc/internal/assert"
)

func assertingFunc(f func(context.Context)) Func {
	return Func(func(ctx context.Context, _ AnyRequest) (AnyResponse, error) {
		f(ctx)
		return NewResponse(&emptypb.Empty{}), nil
	})
}

type loggingInterceptor struct {
	w             io.Writer
	before, after string
}

func (i *loggingInterceptor) Wrap(next Func) Func {
	return Func(func(ctx context.Context, req AnyRequest) (AnyResponse, error) {
		io.WriteString(i.w, i.before)
		defer func() { io.WriteString(i.w, i.after) }()
		return next(ctx, req)
	})
}

func (i *loggingInterceptor) WrapStream(next StreamFunc) StreamFunc {
	return StreamFunc(func(ctx context.Context) (context.Context, Sender, Receiver) {
		io.WriteString(i.w, i.before)
		defer func() { io.WriteString(i.w, i.after) }()
		return next(ctx)
	})
}

type panicSender struct {
	Sender
}

type panicReceiver struct {
	Receiver
}

func TestChain(t *testing.T) {
	out := &bytes.Buffer{}
	chain := NewChain(
		&loggingInterceptor{out, "b1.", "a1"},
		&loggingInterceptor{out, "b2.", "a2."},
	)
	const onion = "b1.b2.a2.a1" // expected execution order
	t.Run("unary", func(t *testing.T) {
		out.Reset()
		var called bool
		next := assertingFunc(func(_ context.Context) {
			called = true
		})
		res, err := chain.Wrap(next)(
			context.Background(),
			NewRequest(&emptypb.Empty{}),
		)
		assert.Nil(t, err, "returned error")
		assert.NotNil(t, res, "returned result")
		assert.Equal(t, out.String(), onion, "execution onion")
		assert.True(t, called, "original Func called")
	})
	t.Run("stream", func(t *testing.T) {
		out.Reset()
		var called bool
		next := StreamFunc(func(ctx context.Context) (context.Context, Sender, Receiver) {
			called = true
			return ctx, &panicSender{}, &panicReceiver{}
		})
		_, sender, receiver := chain.WrapStream(next)(context.Background())
		assert.NotNil(t, sender, "returned sender")
		assert.NotNil(t, receiver, "returned receiver")
		assert.Equal(t, out.String(), onion, "execution onion")
		assert.True(t, called, "original StreamFunc called")
	})
}
