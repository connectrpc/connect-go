// Copyright 2021-2022 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connect

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// An ErrorDetail is a self-describing protobuf message attached to an *Error.
// Error details are sent over the network to clients, which can then work with
// strongly-typed data rather than trying to parse a complex error message.
//
// The ErrorDetail interface is implemented by protobuf's Any type, provided in
// Go by the google.golang.org/protobuf/types/known/anypb package. The
// google.golang.org/genproto/googleapis/rpc/errdetails package contains a
// variety of protobuf messages commonly wrapped in anypb.Any and used as error
// details.
type ErrorDetail interface {
	proto.Message

	MessageName() protoreflect.FullName
	UnmarshalTo(proto.Message) error
}

// An Error captures three key pieces of information: a Code, an underlying Go
// error, and an optional collection of arbitrary protobuf messages called
// "details" (more on those below). Servers send the code, the underlying
// error's Error() output, and details over the wire to clients. Remember that
// the underlying error's message will be sent to clients - take care not to
// leak sensitive information from public APIs!
//
// Service implementations and interceptors should return errors that can be
// cast to an *Error (using the standard library's errors.As). If the returned
// error can't be cast to an *Error, connect will use CodeUnknown and the
// returned error's message.
//
// Error details were introduced before gRPC adopted a formal proposal process,
// so they're not clearly documented anywhere and may differ slightly between
// implementations. Roughly, they're an optional mechanism for servers,
// middleware, and proxies to attach arbitrary protobuf messages to the error
// code and message.
type Error struct {
	code    Code
	err     error
	details []ErrorDetail
	header  http.Header
	trailer http.Header
}

// NewError annotates any Go error with a status code.
func NewError(c Code, underlying error) *Error {
	return &Error{code: c, err: underlying}
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

// Header allows the error to carry additional metadata as HTTP headers.
//
// Unary and client streaming handlers may set headers freely. Server streaming
// and bidirectional streaming handlers should use them only if no messages
// have been sent to the client; when returning errors after sending one or
// more messages, any headers are ignored.
//
// When clients receive errors, Header returns the HTTP response headers.
func (e *Error) Header() http.Header {
	if e.header == nil {
		e.header = make(http.Header)
	}
	return e.header
}

// Trailer allows the error to carry additional metadata as end-of-stream
// trailers. The RPC protocol in use determines whether the trailers are
// written to the network as HTTP trailers or a protocol-specific block of
// metadata.
//
// Unary and streaming handlers may always attach custom trailers to errors.
// When clients receive errors, Trailer returns the union of the HTTP response
// trailers and protocol-specific trailers (if any).
func (e *Error) Trailer() http.Header {
	if e.trailer == nil {
		e.trailer = make(http.Header)
	}
	return e.trailer
}

// CodeOf returns the error's status code if it is or wraps a *connect.Error
// and CodeUnknown otherwise.
func CodeOf(err error) Code {
	if connectErr, ok := asError(err); ok {
		return connectErr.Code()
	}
	return CodeUnknown
}

// errorf calls fmt.Errorf with the supplied template and arguments, then wraps
// the resulting error.
func errorf(c Code, template string, args ...any) *Error {
	return NewError(c, fmt.Errorf(template, args...))
}

// asError uses errors.As to unwrap any error and look for a connect *Error.
func asError(err error) (*Error, bool) {
	var re *Error
	ok := errors.As(err, &re)
	return re, ok
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
	if _, ok := asError(maybeCodedErr); ok {
		return maybeCodedErr
	}
	return NewError(CodeUnknown, maybeCodedErr)
}

// wrapIfContextError applies CodeCanceled or CodeDeadlineExceeded to Go's
// context.Canceled and context.DeadlineExceeded errors, but only if they
// haven't already been wrapped.
func wrapIfContextError(err error) error {
	if _, ok := asError(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return NewError(CodeCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return NewError(CodeDeadlineExceeded, err)
	}
	return err
}
