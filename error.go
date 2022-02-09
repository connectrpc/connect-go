package connect

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// An ErrorDetail is a protobuf message attached to an *Error. Error details
// are sent over the network to clients, which can then work with
// strongly-typed data rather than trying to parse a complex error message.
//
// The ErrorDetail interface is implemented by protobuf's Any type, provided in
// Go by the google.golang.org/protobuf/types/known/anypb package. The
// google.golang.org/genproto/googleapis/rpc/errdetails package contains a
// variety of protobuf messages commonly used as error details.
type ErrorDetail interface {
	proto.Message

	MessageName() protoreflect.FullName
	UnmarshalTo(proto.Message) error
}

// An Error captures three pieces of information: a Code, a human-readable
// message, and an optional collection of arbitrary protobuf messages called
// "details" (more on those below). Servers send the code, message, and details
// over the wire to clients. Connect's Error wraps a standard Go error, using
// the underlying error's Error() string as the message. Take care not to leak
// sensitive information from public APIs!
//
// Protobuf service implementations and Interceptors should return Errors
// (using the Wrap or Errorf functions) rather than plain Go errors. If service
// implementations or Interceptors instead return a plain Go error, connect will
// use AsError to find an Error to send over the wire. If no Error can be
// found, connect will use CodeUnknown and the returned error's message.
//
// Error codes and messages are explained in the gRPC documentation linked
// below. Unfortunately, error details were introduced before gRPC adopted a
// formal proposal process, so they're not clearly documented anywhere and
// may differ slightly between implementations. Roughly, they're an optional
// mechanism for servers, middleware, and proxies to send strongly-typed errors
// and localized messages to clients.
//
// See https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md and
// https://github.com/grpc/grpc/blob/master/doc/statuscodes.md for further
// details.
type Error struct {
	code    Code
	err     error
	details []ErrorDetail
}

// Wrap annotates any error with a status code. If the code is CodeOK, the
// returned error is nil.
func Wrap(c Code, err error) *Error {
	if c == CodeOK {
		return nil
	}
	return &Error{code: c, err: err}
}

// Errorf calls fmt.Errorf with the supplied template and arguments, then wraps
// the resulting error. If the code is CodeOK, the returned error is nil.
func Errorf(c Code, template string, args ...any) *Error {
	return Wrap(c, fmt.Errorf(template, args...))
}

// AsError uses errors.As to unwrap any error and look for a connect *Error.
func AsError(err error) (*Error, bool) {
	var re *Error
	ok := errors.As(err, &re)
	return re, ok
}

func (e *Error) Error() string {
	text := e.err.Error()
	if text == "" {
		return e.code.String()
	}
	return e.code.String() + ": " + text
}

// Unwrap implements errors.Wrapper, which allows errors.Is and errors.As
// access to the underlying error.
func (e *Error) Unwrap() error {
	return e.err
}

// Code returns the error's status code.
func (e *Error) Code() Code {
	if e == nil {
		return CodeOK
	}
	return e.code
}

// Details returns the error's details.
func (e *Error) Details() []ErrorDetail {
	return e.details
}

// AddDetail appends a message to the error's details.
func (e *Error) AddDetail(d ErrorDetail) {
	e.details = append(e.details, d)
}

// CodeOf returns the error's status code if it is or wraps a *connect.Error,
// CodeOK if the error is nil, and CodeUnknown otherwise.
func CodeOf(err error) Code {
	if err == nil {
		return CodeOK
	}
	if cerr, ok := AsError(err); ok {
		return cerr.Code()
	}
	return CodeUnknown
}

// wrapIfUncoded ensures that all errors are wrapped. It leaves already-wrapped
// errors unchanged, uses wrapIfContextError to apply codes to context.Canceled
// and context.DeadlineExceeded, and falls back to wrapping other errors with
// CodeUnknown.
func wrapIfUncoded(err error) error {
	if err == nil {
		return nil
	}
	maybeCodedErr := wrapIfContextError(err)
	if _, ok := AsError(maybeCodedErr); ok {
		return maybeCodedErr
	}
	return Wrap(CodeUnknown, maybeCodedErr)
}

// wrapIfContextError applies CodeCanceled or CodeDeadlineExceeded to Go's
// context.Canceled and context.DeadlineExceeded errors, but only if they
// haven't already been wrapped.
func wrapIfContextError(err error) error {
	if _, ok := AsError(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return Wrap(CodeCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Wrap(CodeDeadlineExceeded, err)
	}
	return err
}
