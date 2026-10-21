// Copyright 2021-2026 The Connect Authors
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
	"os"
	"strings"
	"syscall"

	connectv2 "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	defaultAnyResolverPrefix = "type.googleapis.com/"
)

// Messages of the unexported net/http errors for a write to a stream the
// client has abandoned.
const (
	clientDisconnectedMessage = "client disconnected"
	streamClosedMessage       = "http2: stream closed"
)

var (
	// errNotModified signals Connect-protocol responses to GET requests to use the
	// 304 Not Modified HTTP error code.
	errNotModified = errors.New("not modified")
	// errNotModifiedClient wraps ErrNotModified for use client-side.
	errNotModifiedClient = fmt.Errorf("HTTP 304: %w", errNotModified)
)

// An ErrorDetail is a self-describing Protobuf message attached to an [*Error].
// Error details are sent over the network to clients, which can then work with
// strongly-typed data rather than trying to parse a complex error message. For
// example, you might use details to send a localized error message or retry
// parameters to the client.
//
// The [google.golang.org/genproto/googleapis/rpc/errdetails] package contains a
// variety of Protobuf messages commonly used as error details.
type ErrorDetail struct {
	pbAny    *anypb.Any
	pbInner  proto.Message // if nil, must be extracted from pbAny
	wireJSON string        // preserve human-readable JSON
}

// NewErrorDetail constructs a new error detail. If msg is an *[anypb.Any] then
// it is used as is. Otherwise, it is first marshalled into an *[anypb.Any]
// value. This returns an error if msg cannot be marshalled.
func NewErrorDetail(msg proto.Message) (*ErrorDetail, error) {
	// If it's already an Any, don't wrap it inside another.
	if pb, ok := msg.(*anypb.Any); ok {
		return &ErrorDetail{pbAny: pb}, nil
	}
	pb, err := anypb.New(msg)
	if err != nil {
		return nil, err
	}
	return &ErrorDetail{pbAny: pb, pbInner: msg}, nil
}

// Type is the fully-qualified name of the detail's Protobuf message (for
// example, acme.foo.v1.FooDetail).
func (d *ErrorDetail) Type() string {
	// proto.Any tries to make messages self-describing by using type URLs rather
	// than plain type names, but there aren't any descriptor registries
	// deployed. With the current state of the `Any` code, it's not possible to
	// build a useful type registry either. To hide this from users, we should
	// trim the URL prefix is added to the type name.
	//
	// If we ever want to support remote registries, we can add an explicit
	// `TypeURL` method.
	return typeNameForURL(d.pbAny.GetTypeUrl())
}

// Bytes returns a copy of the Protobuf-serialized detail.
func (d *ErrorDetail) Bytes() []byte {
	out := make([]byte, len(d.pbAny.GetValue()))
	copy(out, d.pbAny.GetValue())
	return out
}

// Value uses the Protobuf runtime's package-global registry to unmarshal the
// Detail into a strongly-typed message. Typically, clients use Go type
// assertions to cast from the proto.Message interface to concrete types.
func (d *ErrorDetail) Value() (proto.Message, error) {
	if d.pbInner != nil {
		// We clone it so that if the caller mutates the returned value,
		// they don't inadvertently corrupt this error detail value.
		return proto.Clone(d.pbInner), nil
	}
	return d.pbAny.UnmarshalNew()
}

// An Error captures four key pieces of information: a [Code], an underlying Go
// error, a map of metadata, and an optional collection of arbitrary Protobuf
// messages called "details" (more on those below). Servers send the code, the
// underlying error's Error() output, the metadata, and details over the wire
// to clients. Remember that the underlying error's message will be sent to
// clients - take care not to leak sensitive information from public APIs!
//
// Service implementations and interceptors should return errors that can be
// cast to an [*Error] (using the standard library's [errors.As]). If the returned
// error can't be cast to an [*Error], connect will use [CodeUnknown] and the
// returned error's message.
//
// Error details are an optional mechanism for servers, interceptors, and
// proxies to attach arbitrary Protobuf messages to the error code and message.
// They're a clearer and more performant alternative to HTTP header
// microformats. See [the documentation on errors] for more details.
//
// [the documentation on errors]: https://connectrpc.com/docs/go/errors
type Error struct {
	code    Code
	err     error
	details []*ErrorDetail
	meta    http.Header
	wireErr bool
}

// NewError annotates any Go error with a status code.
func NewError(c Code, underlying error) *Error {
	return &Error{code: c, err: underlying}
}

// NewWireError is similar to [NewError], but the resulting *Error returns true
// when tested with [IsWireError].
//
// This is useful for clients trying to propagate partial failures from
// streaming RPCs. Often, these RPCs include error information in their
// response messages (for example, [gRPC server reflection] and
// OpenTelemetry's [OTLP]). Clients propagating these errors up the stack
// should use NewWireError to clarify that the error code, message, and details
// (if any) were explicitly sent by the server rather than inferred from a
// lower-level networking error or timeout.
//
// [gRPC server reflection]: https://github.com/grpc/grpc/blob/v1.49.2/src/proto/grpc/reflection/v1alpha/reflection.proto#L132-L136
// [OTLP]: https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/protocol/otlp.md#partial-success
func NewWireError(c Code, underlying error) *Error {
	err := NewError(c, underlying)
	err.wireErr = true
	return err
}

// IsWireError checks whether the error was returned by the server, as opposed
// to being synthesized by the client.
//
// Clients may find this useful when deciding how to propagate errors. For
// example, an RPC-to-HTTP proxy might expose a server-sent CodeUnknown as an
// HTTP 500 but a client-synthesized CodeUnknown as a 503.
//
// Handlers will strip [Error.Meta] headers propagated from wire errors to avoid
// leaking response headers. To propagate headers recreate the error as a
// non-wire error.
func IsWireError(err error) bool {
	connectErr, ok := asError(err)
	return ok && connectErr.wireErr
}

// NewNotModifiedError indicates that the requested resource hasn't changed. It
// should be used only when handlers wish to respond to conditional HTTP GET
// requests with a 304 Not Modified. In all other circumstances, including all
// RPCs using the gRPC or gRPC-Web protocols, it's equivalent to sending an
// error with [CodeUnknown]. The supplied headers should include Etag,
// Cache-Control, or any other headers required by [RFC 9110 § 15.4.5].
//
// Clients should check for this error using [IsNotModifiedError].
//
// [RFC 9110 § 15.4.5]: https://httpwg.org/specs/rfc9110.html#status.304
func NewNotModifiedError(headers http.Header) *Error {
	err := NewError(CodeUnknown, errNotModified)
	if headers != nil {
		err.meta = headers
	}
	return err
}

func (e *Error) Error() string {
	code := e.Code()
	message := e.Message()
	if message == "" {
		return code.String()
	}
	return code.String() + ": " + message
}

// Message returns the underlying error message. It may be empty if the
// original error was created with a status code and a nil error.
func (e *Error) Message() string {
	if e != nil && e.err != nil {
		return e.err.Error()
	}
	return ""
}

// Unwrap allows [errors.Is] and [errors.As] access to the underlying error.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// Code returns the error's status code.
func (e *Error) Code() Code {
	if e == nil {
		return CodeUnknown
	}
	return e.code
}

// Details returns the error's details.
func (e *Error) Details() []*ErrorDetail {
	return e.details
}

// AddDetail appends to the error's details.
func (e *Error) AddDetail(d *ErrorDetail) {
	e.details = append(e.details, d)
}

// Meta allows the error to carry additional information as key-value pairs.
//
// Protocol-specific headers and trailers may be removed to avoid breaking
// protocol semantics. For example, Content-Length and Content-Type headers
// won't be propagated. See the documentation for each protocol for more
// datails.
//
// When clients receive errors, the metadata contains the union of the HTTP
// headers and the protocol-specific trailers (either HTTP trailers or in-body
// metadata).
func (e *Error) Meta() http.Header {
	if e.meta == nil {
		e.meta = make(http.Header)
	}
	return e.meta
}

// IsNotModifiedError checks whether the supplied error indicates that the
// requested resource hasn't changed. It only returns true if the server used
// [NewNotModifiedError] in response to a Connect-protocol RPC made with an
// HTTP GET.
func IsNotModifiedError(err error) bool {
	return errors.Is(err, errNotModified)
}

// errorf calls fmt.Errorf with the supplied template and arguments, then wraps
// the resulting error.
func errorf(c Code, template string, args ...any) *Error {
	return NewError(c, fmt.Errorf(template, args...))
}

// asError uses errors.As to unwrap any error and look for a non-nil connect *Error.
func asError(err error) (*Error, bool) {
	var connectErr *Error
	ok := errors.As(err, &connectErr)
	return connectErr, ok && connectErr != nil // Protect against typed nils (*Error)(nil).
}

// wrapIfContextError applies CodeCanceled or CodeDeadlineExceeded to Go's
// context.Canceled and context.DeadlineExceeded errors, but only if they
// haven't already been wrapped.
func wrapIfContextError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := asError(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return NewError(CodeCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return NewError(CodeDeadlineExceeded, err)
	}
	// Ick, some dial errors can be returned as os.ErrDeadlineExceeded
	// instead of context.DeadlineExceeded :(
	// https://github.com/golang/go/issues/64449
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return NewError(CodeDeadlineExceeded, err)
	}
	// The request context is cancelled from a different goroutine than the one
	// failing the write, so a write can fail while ctx.Err() is still nil.
	// https://github.com/golang/go/issues/52183
	if isClientDisconnected(err) {
		return NewError(CodeCanceled, err)
	}
	return err
}

// isClientDisconnected reports whether err is a write failure caused by the
// client going away. HTTP/2 has its own errors for this, other transports
// surface the connection's error.
func isClientDisconnected(err error) bool {
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	msg := err.Error()
	return msg == clientDisconnectedMessage || msg == streamClosedMessage
}

// toConnectv2 packs this v1 [*Error] into the matching [*connectv2.Error] for
// dispatch through the v2 runtime. The local cause chain becomes
// [connectv2.Error.WithCause] and details are forwarded as their underlying
// [*anypb.Any] values.
//
// A nil receiver yields a nil result.
//
// v1's wireErr flag is intentionally NOT translated to v2's
// [connectv2.Error.WithRemote]: v2 scrubs remote errors to CodeInternal at
// the encode boundary (the connect-go #222 fix), but v1 has always
// propagated [NewWireError]-tagged errors verbatim. Preserving v1's
// behaviour is one of the "v1 keeps relaxed checks" carve-outs.
func (e *Error) toConnectv2() *connectv2.Error {
	if e == nil {
		return nil
	}
	v2err := connectv2.NewError(e.code, e.Message())
	if e.err != nil {
		v2err = v2err.WithCause(e.err)
	}
	for _, detail := range e.details {
		if detail != nil && detail.pbAny != nil {
			v2err = v2err.WithDetail(&connectv2.ErrorDetail{
				Type:  detail.Type(),
				Value: detail.pbAny.GetValue(),
				Debug: []byte(detail.wireJSON),
			})
		}
	}
	return v2err
}

// errorFromConnectv2 unpacks a v2 error value into a v1 [*Error]. It accepts
// any error: callers pass through whatever the v2 runtime returned and get
// back a v1 [*Error] when one was present in the chain, or nil otherwise.
//
// The v2 error's cause chain is preserved (so [errors.Is] still finds
// [context.Canceled], [context.DeadlineExceeded], and any other wrapped
// sentinels). The wire-error flag is mirrored from [connectv2.Error.IsRemote].
func errorFromConnectv2(err error) *Error {
	if err == nil {
		return nil
	}
	var v2err *connectv2.Error
	if !errors.As(err, &v2err) {
		return nil
	}
	v1err := &Error{
		code:    v2err.Code(),
		wireErr: v2err.IsRemote(),
	}
	cause := v2err.Unwrap()
	switch {
	case cause != nil:
		msg := v2err.Message()
		// v2 keeps the cause out of the message, while v1 formats it in with
		// ": %w". Restore v1's message for errors raised locally.
		if causeMsg := cause.Error(); !v2err.IsRemote() && !strings.HasSuffix(msg, causeMsg) {
			if msg == "" {
				msg = causeMsg
			} else {
				msg += ": " + causeMsg
			}
		}
		v1err.err = &v2BoundaryError{msg: msg, cause: cause}
	case v2err.Message() != "":
		v1err.err = errors.New(v2err.Message())
	}
	for _, detail := range v2err.Details() {
		v1err.details = append(v1err.details, &ErrorDetail{
			pbAny:    connectproto.ErrorDetailToAny(detail),
			wireJSON: string(detail.Debug),
		})
	}
	return v1err
}

// v2BoundaryError carries a v2 error's message and cause through v1's
// [Error.err] field. v1's Message() reads its Error(); errors.Is and
// errors.As traverse through Unwrap to the original cause so context
// sentinels remain discoverable.
type v2BoundaryError struct {
	msg   string
	cause error
}

func (e *v2BoundaryError) Error() string { return e.msg }
func (e *v2BoundaryError) Unwrap() error { return e.cause }

func typeNameForURL(url string) string {
	return url[strings.LastIndexByte(url, '/')+1:]
}
