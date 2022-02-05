package connect

import (
	"context"
)

// ErrorInterceptor modifies handler-side errors before they're
// written to the network and client-side errors after they're read from the
// network.
//
// If your application uses custom Go error implementations to convey more
// information than a simple message, this interceptor lets you automatically
// propagate that information over the network.
type ErrorInterceptor struct {
	toWire   func(error) error
	fromWire func(error) error
}

var _ Interceptor = (*ErrorInterceptor)(nil)

// NewErrorInterceptor constructs an error-translating interceptor.
//
// Use toWire to convert your custom type to an *Error (using ErrorDetails to
// capture any data beyond a code and message). ToWire acts on errors which
// will be written to the network: (1) errors returned from unary handler
// implementations, and (2) errors passed to Sender.Close.
//
// Use fromWire to modify errors before they reach your application code. For
// example, you could convert an *Error with ErrorDetails into a custom error
// implementation used throughout your application. FromWire acts on: (1)
// errors received by unary client methods, (2) errors returned from
// Receiver.Receive and Receiver.Close, and (3) errors returned from
// Sender.Send and Sender.Close.
//
// Nil function pointers are treated as no-ops.
func NewErrorInterceptor(
	toWire func(error) error,
	fromWire func(error) error,
) *ErrorInterceptor {
	interceptor := ErrorInterceptor{
		toWire:   toWire,
		fromWire: fromWire,
	}
	if interceptor.toWire == nil {
		interceptor.toWire = func(err error) error { return err }
	}
	if interceptor.fromWire == nil {
		interceptor.fromWire = func(err error) error { return err }
	}
	return &interceptor
}

// Wrap implements Interceptor.
func (i *ErrorInterceptor) Wrap(next Func) Func {
	return Func(func(ctx context.Context, req AnyRequest) (AnyResponse, error) {
		res, err := next(ctx, req)
		if err == nil {
			return res, nil
		}
		if req.Spec().IsClient {
			return nil, i.fromWire(err)
		}
		return nil, i.toWire(err)
	})
}

// WrapContext implements Interceptor with a no-op.
func (i *ErrorInterceptor) WrapContext(ctx context.Context) context.Context {
	return ctx
}

// WrapSender implements Interceptor. For handlers, it translates errors to the
// wire format. For clients, it's a no-op.
func (i *ErrorInterceptor) WrapSender(_ context.Context, sender Sender) Sender {
	return &errorTranslatingSender{
		Sender:   sender,
		toWire:   i.toWire,
		fromWire: i.fromWire,
	}
}

// WrapReceiver implements Interceptor. For clients, it translates errors from
// the wire format. For handlers, it's a no-op.
func (i *ErrorInterceptor) WrapReceiver(_ context.Context, receiver Receiver) Receiver {
	return &errorTranslatingReceiver{Receiver: receiver, fromWire: i.fromWire}
}

type errorTranslatingSender struct {
	Sender

	toWire   func(error) error
	fromWire func(error) error
}

func (s *errorTranslatingSender) Send(msg any) error {
	return s.fromWire(s.Sender.Send(msg))
}

func (s *errorTranslatingSender) Close(err error) error {
	sendErr := s.Sender.Close(s.toWire(err))
	return s.fromWire(sendErr)
}

type errorTranslatingReceiver struct {
	Receiver

	fromWire func(error) error
}

func (r *errorTranslatingReceiver) Receive(msg any) error {
	if err := r.Receiver.Receive(msg); err != nil {
		return r.fromWire(err)
	}
	return nil
}

func (r *errorTranslatingReceiver) Close() error {
	return r.fromWire(r.Receiver.Close())
}
