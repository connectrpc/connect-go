package rerpc

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/akshayjshah/rerpc/internal/assert"
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

func TestChain(t *testing.T) {
	out := &bytes.Buffer{}
	chain := NewChain(
		&loggingInterceptor{out, "b1.", "a1"},
		&loggingInterceptor{out, "b2.", "a2."},
	)
	const onion = "b1.b2.a2.a1" // expected execution order
	var called bool
	next := assertingFunc(func(_ context.Context) {
		called = true
	})
	res, err := chain.Wrap(next)(context.Background(), &emptypb.Empty{})
	assert.Nil(t, err, "returned error")
	assert.NotNil(t, res, "returned result")
	assert.Equal(t, out.String(), onion, "execution onion")
	assert.True(t, called, "original Func called")
}

func TestClampTimeout(t *testing.T) {
	const min, max = time.Second, 10 * time.Second
	clamp := ClampTimeout(min, max)
	t.Run("min", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), min-1)
		defer cancel()
		var called bool
		call := clamp.Wrap(assertingFunc(func(_ context.Context) {
			called = true
		}))
		res, err := call(ctx, &emptypb.Empty{})
		assert.NotNil(t, err, "clamp error")
		assert.Equal(t, CodeOf(err), CodeDeadlineExceeded, "error code")
		assert.Nil(t, res, "proto result")
		assert.False(t, called, "original Func called")
	})
	t.Run("max", func(t *testing.T) {
		var called bool
		call := clamp.Wrap(assertingFunc(func(ctx context.Context) {
			called = true
			deadline, ok := ctx.Deadline()
			assert.True(t, ok, "context has deadline")
			timeout := time.Until(deadline)
			t.Log(timeout)
			assert.True(t, timeout <= max, "timeout clamped to max")
		}))

		called = false
		ctx, cancel := context.WithTimeout(context.Background(), 2*max)
		defer cancel()
		res, err := call(ctx, &emptypb.Empty{})
		assert.Nil(t, err, "long func error")
		assert.NotNil(t, res, "long func result")
		assert.True(t, called, "long func called")

		called = false
		res, err = call(context.Background(), &emptypb.Empty{})
		assert.Nil(t, err, "unbounded func error")
		assert.NotNil(t, res, "unbounded func result")
		assert.True(t, called, "unbounded func called")
	})
}

func TestRecover(t *testing.T) {
	const msg = "panic at the disco"
	var called bool
	log := func(_ context.Context, val interface{}) {
		called = true
		assert.Equal(t, val, msg, "panic value")
	}
	r := Recover(log)

	called = false
	f := r.Wrap(assertingFunc(func(_ context.Context) {
		panic(msg)
	}))
	res, err := f(context.Background(), &emptypb.Empty{})
	assert.True(t, called, "logged panic")
	assert.Nil(t, err, "error")
	assert.Nil(t, res, "result")
}
