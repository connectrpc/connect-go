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

func assertingCall(f func(context.Context)) UnaryCall {
	return UnaryCall(func(ctx context.Context, _, _ proto.Message) error {
		f(ctx)
		return nil
	})
}

func assertingHandler(f func(context.Context)) UnaryHandler {
	return UnaryHandler(func(ctx context.Context, _ proto.Message) (proto.Message, error) {
		f(ctx)
		return &emptypb.Empty{}, nil
	})
}

type loggingInterceptor struct {
	w             io.Writer
	before, after string
}

func (i *loggingInterceptor) WrapCall(next UnaryCall) UnaryCall {
	return UnaryCall(func(ctx context.Context, req, res proto.Message) error {
		io.WriteString(i.w, i.before)
		defer func() { io.WriteString(i.w, i.after) }()
		return next(ctx, req, res)
	})
}

func (i *loggingInterceptor) WrapHandler(next UnaryHandler) UnaryHandler {
	return UnaryHandler(func(ctx context.Context, req proto.Message) (proto.Message, error) {
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
	t.Run("call", func(t *testing.T) {
		out.Reset()
		var called bool
		next := assertingCall(func(_ context.Context) {
			called = true
		})
		err := chain.WrapCall(next)(context.Background(), &emptypb.Empty{}, &emptypb.Empty{})
		assert.Nil(t, err, "call error")
		assert.Equal(t, out.String(), onion, "execution onion")
		assert.True(t, called, "original UnaryCall called")
	})
	t.Run("handler", func(t *testing.T) {
		out.Reset()
		var called bool
		next := assertingHandler(func(_ context.Context) {
			called = true
		})
		res, err := chain.WrapHandler(next)(context.Background(), &emptypb.Empty{})
		assert.Nil(t, err, "handler error")
		assert.NotNil(t, res, "handler result")
		assert.Equal(t, out.String(), onion, "execution onion")
		assert.True(t, called, "original UnaryHandler called")
	})
}

func TestClampTimeout(t *testing.T) {
	const min, max = time.Second, 10 * time.Second
	clamp := ClampTimeout(min, max)
	t.Run("call", func(t *testing.T) {
		t.Run("short_circuit", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), min-1)
			defer cancel()
			var called bool
			call := clamp.WrapCall(assertingCall(func(_ context.Context) {
				called = true
			}))
			err := call(ctx, &emptypb.Empty{}, &emptypb.Empty{})
			assert.NotNil(t, err, "clamp error")
			assert.Equal(t, CodeOf(err), CodeDeadlineExceeded, "error code")
			assert.False(t, called, "original UnaryCall called")
		})
		t.Run("clamp", func(t *testing.T) {
			var called bool
			call := clamp.WrapCall(assertingCall(func(ctx context.Context) {
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
			err := call(ctx, &emptypb.Empty{}, &emptypb.Empty{})
			assert.Nil(t, err, "long call error")
			assert.True(t, called, "long UnaryCall called")

			called = false
			err = call(context.Background(), &emptypb.Empty{}, &emptypb.Empty{})
			assert.Nil(t, err, "unbounded call error")
			assert.True(t, called, "unbounded UnaryCall called")
		})
	})
	t.Run("handler", func(t *testing.T) {
		t.Run("short_circuit", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), min-1)
			defer cancel()
			var called bool
			call := clamp.WrapHandler(assertingHandler(func(_ context.Context) {
				called = true
			}))
			res, err := call(ctx, &emptypb.Empty{})
			assert.NotNil(t, err, "clamp error")
			assert.Equal(t, CodeOf(err), CodeDeadlineExceeded, "error code")
			assert.Nil(t, res, "handler proto result")
			assert.False(t, called, "original UnaryCall called")
		})
		t.Run("clamp", func(t *testing.T) {
			var called bool
			call := clamp.WrapHandler(assertingHandler(func(ctx context.Context) {
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
			assert.Nil(t, err, "long handler error")
			assert.NotNil(t, res, "long handler result")
			assert.True(t, called, "long handler called")

			called = false
			res, err = call(context.Background(), &emptypb.Empty{})
			assert.Nil(t, err, "unbounded handler error")
			assert.NotNil(t, res, "unbounded handler result")
			assert.True(t, called, "unbounded handler called")
		})
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
	call := r.WrapCall(assertingCall(func(_ context.Context) {
		panic(msg)
	}))
	err := call(context.Background(), &emptypb.Empty{}, &emptypb.Empty{})
	assert.True(t, called, "logged panic in call")
	assert.Nil(t, err, "call error")

	called = false
	handler := r.WrapHandler(assertingHandler(func(_ context.Context) {
		panic(msg)
	}))
	res, err := handler(context.Background(), &emptypb.Empty{})
	assert.True(t, called, "logged panic in handler")
	assert.Nil(t, err, "handler error")
	assert.Nil(t, res, "handler result")
}
