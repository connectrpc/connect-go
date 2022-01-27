package rerpc

import (
	"context"
)

type AnyRequest interface {
	Any() any
	Spec() Specification
	Header() Header

	// Only internal implementations, so we can add methods without breaking
	// backward compatibility.
	internalOnly()
}

type AnyResponse interface {
	Any() any
	Header() Header

	// Only internal implementations, so we can add methods without breaking
	// backward compatibility.
	internalOnly()
}

// Func is the generic signature of a unary RPC. Interceptors wrap Funcs.
//
// The type of the request and response struct depend on the codec being used.
// When using protobuf, they'll always be proto.Message implementations.
type Func func(context.Context, AnyRequest) (AnyResponse, error)

// An Interceptor adds logic to a generated handler or client, like the
// decorators or middleware you may have seen in other libraries. Interceptors
// may replace the context, mutate the request, mutate the response, handle the
// returned error, retry, recover from panics, emit logs and metrics, or do
// nearly anything else.
//
// See Chain for an example of interceptor use.
type Interceptor interface {
	// Wrap adds logic to a unary procedure. The returned Func must be safe to
	// call concurrently.
	Wrap(Func) Func

	// WrapContext, WrapSender, and WrapReceiver work together to add logic to
	// streaming procedures. Stream interceptors work in phases. First, each
	// interceptor may wrap the request context. Then, the reRPC runtime
	// constructs a (Sender, Receiver) pair. Finally, each interceptor may wrap
	// the Sender and/or Receiver. For example, the flow within a Handler looks
	// like this:
	//
	//   func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	//     ctx := r.Context()
	//     if ic := h.interceptor; ic != nil {
	//       ctx = ic.WrapContext(ctx)
	//     }
	//     sender, receiver := h.newStream(w, r.WithContext(ctx))
	//     if ic := h.interceptor; ic != nil {
	//       sender = ic.WrapSender(ctx, sender)
	//       receiver = ic.WrapReceiver(ctx, receiver)
	//     }
	//     h.serveStream(sender, receiver)
	//   }
	//
	// Senders and Receivers don't need to be safe for concurrent use.
	WrapContext(context.Context) context.Context
	WrapSender(context.Context, Sender) Sender
	WrapReceiver(context.Context, Receiver) Receiver
}

// A UnaryInterceptorFunc is a simple Interceptor implementation that only
// wraps unary RPCs. It has no effect on client, server, or bidirectional
// streaming RPCs. See CallMetadata for an example.
type UnaryInterceptorFunc func(Func) Func

// Wrap implements Interceptor by applying the interceptor function.
func (f UnaryInterceptorFunc) Wrap(next Func) Func { return f(next) }

// WrapContext implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapContext(ctx context.Context) context.Context {
	return ctx
}

// WrapSender implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapSender(_ context.Context, s Sender) Sender {
	return s
}

// WrapReceiver implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapReceiver(_ context.Context, r Receiver) Receiver {
	return r
}

// A Chain composes multiple interceptors into one.
type Chain struct {
	interceptors []Interceptor
}

var _ Interceptor = (*Chain)(nil)

// NewChain composes multiple interceptors into one.
//
// Unary interceptors compose like an onion. The first interceptor provided is
// the outermost layer of the onion: it acts first on the context and request,
// and last on the response and error.
//
// Stream interceptors also behave like an onion: the first interceptor
// provided is the first to wrap the context and is the outermost wrapper for
// the (Sender, Receiver) pair. It's the first to see sent messages and the
// last to see received messages.
//
// Applied to client and handler, NewChain(A, B, ..., Y, Z) produces:
//
//        client.Send()     client.Receive()
//              |                 ^
//              v                 |
//           A ---               --- A
//           B ---               --- B
//             ...               ...
//           Y ---               --- Y
//           Z ---               --- Z
//              |                 ^
//              v                 |
//           network            network
//              |                 ^
//              v                 |
//           A ---               --- A
//           B ---               --- B
//             ...               ...
//           Y ---               --- Y
//           Z ---               --- Z
//              |                 ^
//              v                 |
//       handler.Receive() handler.Send()
//              |                 ^
//              |                 |
//              -> handler logic --
//
// Note that in clients, the Sender handles the request message(s) and the
// Receiver handles the response message(s). For handlers, it's the reverse.
// Depending on your interceptor's logic, you may need to wrap one side of the
// stream on the clients and the other side on handlers. See the implementation
// of HeaderInterceptor for an example.
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

// WrapContext implements Interceptor.
func (c *Chain) WrapContext(ctx context.Context) context.Context {
	for i := len(c.interceptors) - 1; i >= 0; i-- {
		if interceptor := c.interceptors[i]; interceptor != nil {
			ctx = interceptor.WrapContext(ctx)
		}
	}
	return ctx
}

// WrapSender implements Interceptor.
func (c *Chain) WrapSender(ctx context.Context, sender Sender) Sender {
	// When we're wrapping senders on the handler side, we need to wrap in the
	// opposite order.
	if sender.Spec().IsClient {
		for i := len(c.interceptors) - 1; i >= 0; i-- {
			if interceptor := c.interceptors[i]; interceptor != nil {
				sender = interceptor.WrapSender(ctx, sender)
			}
		}
		return sender
	}
	for i := 0; i < len(c.interceptors); i++ {
		if interceptor := c.interceptors[i]; interceptor != nil {
			sender = interceptor.WrapSender(ctx, sender)
		}
	}
	return sender
}

// WrapReceiver implements Interceptor.
func (c *Chain) WrapReceiver(ctx context.Context, receiver Receiver) Receiver {
	for i := len(c.interceptors) - 1; i >= 0; i-- {
		if interceptor := c.interceptors[i]; interceptor != nil {
			receiver = interceptor.WrapReceiver(ctx, receiver)
		}
	}
	return receiver
}
