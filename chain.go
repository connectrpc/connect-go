package connect

import (
	"context"
)

// A Chain composes multiple interceptors into one.
type chain struct {
	interceptors []Interceptor
}

var _ Interceptor = (*chain)(nil)

// NewChain composes multiple interceptors into one.
func newChain(interceptors []Interceptor) *chain {
	return &chain{interceptors}
}

func (c *chain) Wrap(next Func) Func {
	// We need to wrap in reverse order to have the first interceptor from
	// the slice act first.
	for i := len(c.interceptors) - 1; i >= 0; i-- {
		if interceptor := c.interceptors[i]; interceptor != nil {
			next = interceptor.Wrap(next)
		}
	}
	return next
}

func (c *chain) WrapContext(ctx context.Context) context.Context {
	for i := len(c.interceptors) - 1; i >= 0; i-- {
		if interceptor := c.interceptors[i]; interceptor != nil {
			ctx = interceptor.WrapContext(ctx)
		}
	}
	return ctx
}

func (c *chain) WrapSender(ctx context.Context, sender Sender) Sender {
	if sender.Spec().IsClient {
		for i := len(c.interceptors) - 1; i >= 0; i-- {
			if interceptor := c.interceptors[i]; interceptor != nil {
				sender = interceptor.WrapSender(ctx, sender)
			}
		}
		return sender
	}
	// When we're wrapping senders on the handler side, we need to wrap in the
	// opposite order. See TestOnionOrderingEndToEnd.
	for i := 0; i < len(c.interceptors); i++ {
		if interceptor := c.interceptors[i]; interceptor != nil {
			sender = interceptor.WrapSender(ctx, sender)
		}
	}
	return sender
}

func (c *chain) WrapReceiver(ctx context.Context, receiver Receiver) Receiver {
	for i := len(c.interceptors) - 1; i >= 0; i-- {
		if interceptor := c.interceptors[i]; interceptor != nil {
			receiver = interceptor.WrapReceiver(ctx, receiver)
		}
	}
	return receiver
}
