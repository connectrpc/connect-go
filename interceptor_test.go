package rerpc

import (
	"bytes"
	"context"
	"io"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/rerpc/rerpc/internal/assert"
)

func assertingFunc(f func(context.Context)) Func {
	return Func(func(ctx context.Context, _ proto.Message) (proto.Message, error) {
		f(ctx)
		return &emptypb.Empty{}, nil
	})
}

type loggingInterceptor struct {
	w             io.Writer
	before, after string
}

func (i *loggingInterceptor) Wrap(next Func) Func {
	return Func(func(ctx context.Context, req proto.Message) (proto.Message, error) {
		io.WriteString(i.w, i.before)
		defer func() { io.WriteString(i.w, i.after) }()
		return next(ctx, req)
	})
}

func (i *loggingInterceptor) WrapHandlerStream(next HandlerStreamFunc) HandlerStreamFunc {
	return HandlerStreamFunc(func(ctx context.Context, stream Stream) {
		io.WriteString(i.w, i.before)
		defer func() { io.WriteString(i.w, i.after) }()
		next(ctx, stream)
	})
}

func (i *loggingInterceptor) WrapCallStream(next CallStreamFunc) CallStreamFunc {
	return CallStreamFunc(func(ctx context.Context) Stream {
		io.WriteString(i.w, i.before)
		defer func() { io.WriteString(i.w, i.after) }()
		return next(ctx)
	})
}

type panicStream struct {
	Stream
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
		res, err := chain.Wrap(next)(context.Background(), &emptypb.Empty{})
		assert.Nil(t, err, "returned error")
		assert.NotNil(t, res, "returned result")
		assert.Equal(t, out.String(), onion, "execution onion")
		assert.True(t, called, "original Func called")
	})
	t.Run("handler_stream", func(t *testing.T) {
		out.Reset()
		var called bool
		next := HandlerStreamFunc(func(_ context.Context, _ Stream) {
			called = true
		})
		chain.WrapHandlerStream(next)(context.Background(), &panicStream{})
		assert.Equal(t, out.String(), onion, "execution onion")
		assert.True(t, called, "original HandlerStreamFunc called")
	})
	t.Run("call_stream", func(t *testing.T) {
		out.Reset()
		var called bool
		next := CallStreamFunc(func(_ context.Context) Stream {
			called = true
			return &panicStream{}
		})
		stream := chain.WrapCallStream(next)(context.Background())
		assert.NotNil(t, stream, "returned stream")
		assert.Equal(t, out.String(), onion, "execution onion")
		assert.True(t, called, "original CallStreamFunc called")
	})
}
