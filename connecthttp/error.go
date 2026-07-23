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

package connecthttp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
)

const commonErrorsURL = "https://connectrpc.com/docs/go/common-errors"

var (
	// errNotModified signals Connect-protocol responses to GET requests to use the
	// 304 Not Modified HTTP error code.
	errNotModified = errors.New("not modified")
	// errNotModifiedClient wraps ErrNotModified for use client-side.
	errNotModifiedClient = fmt.Errorf("HTTP 304: %w", errNotModified)
)

// NewNotModifiedError indicates that the requested resource hasn't changed. It
// should be used only when handlers wish to respond to conditional HTTP GET
// requests with a 304 Not Modified. In all other circumstances, including all
// RPCs using the gRPC or gRPC-Web protocols, it's equivalent to sending an
// error with [connect.CodeUnknown]. Handlers should set Etag, Cache-Control,
// or any other headers required by [RFC 9110 § 15.4.5] on the response header
// metadata reached via [connect.CallInfoForServerContext].
//
// Clients should check for this error using [IsNotModifiedError].
//
// [RFC 9110 § 15.4.5]: https://httpwg.org/specs/rfc9110.html#status.304
func NewNotModifiedError() *connect.Error {
	return connect.Errorf(connect.CodeUnknown, "%s", errNotModified).WithCause(errNotModified)
}

// IsNotModifiedError checks whether the supplied error indicates that the
// requested resource hasn't changed. It only returns true if the server used
// [NewNotModifiedError] in response to a Connect-protocol RPC made with an
// HTTP GET.
func IsNotModifiedError(err error) bool {
	return errors.Is(err, errNotModified)
}

// asError uses errors.As to unwrap any error and look for a connect *connect.Error.
func asError(err error) (*connect.Error, bool) {
	var connectErr *connect.Error
	ok := errors.As(err, &connectErr)
	return connectErr, ok
}

// wrapIfUncoded ensures that all errors are wrapped. It leaves already-wrapped
// errors unchanged, uses wrapIfContextError to apply codes to context.Canceled
// and context.DeadlineExceeded, and falls back to wrapping other errors with
// connect.CodeUnknown.
func wrapIfUncoded(err error) error {
	if err == nil {
		return nil
	}
	maybeCodedErr := wrapIfContextError(err)
	if _, ok := asError(maybeCodedErr); ok {
		return maybeCodedErr
	}
	return connect.NewError(connect.CodeUnknown, maybeCodedErr.Error()).WithCause(maybeCodedErr)
}

// scrubHandlerError converts a handler's returned error into the wire
// verdict: only a locally authored *connect.Error keeps its code, message,
// and details. Remote errors become [connect.CodeInternal] and other errors
// [connect.CodeUnknown] (context errors keep their codes), all with no
// message. The original error stays attached as a local-only cause.
func scrubHandlerError(err error) error {
	if err == nil {
		return nil
	}
	if cerr, ok := asError(err); ok {
		if cerr.IsRemote() {
			return connect.NewError(connect.CodeInternal, "").WithCause(err)
		}
		return err
	}
	code := connect.CodeUnknown
	switch {
	case errors.Is(err, context.Canceled):
		code = connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		code = connect.CodeDeadlineExceeded
	}
	return connect.NewError(code, "").WithCause(err)
}

// wrapIfContextError applies connect.CodeCanceled or connect.CodeDeadlineExceeded to Go's
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
		return connect.NewError(connect.CodeCanceled, err.Error()).WithCause(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err.Error()).WithCause(err)
	}
	// Ick, some dial errors can be returned as os.ErrDeadlineExceeded
	// instead of context.DeadlineExceeded :(
	// https://github.com/golang/go/issues/64449
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err.Error()).WithCause(err)
	}
	return err
}

// wrapIfContextDone wraps errors with connect.CodeCanceled or connect.CodeDeadlineExceeded
// if the context is done. It leaves already-wrapped errors unchanged.
func wrapIfContextDone(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	err = wrapIfContextError(err)
	if _, ok := asError(err); ok {
		return err
	}
	ctxErr := ctx.Err()
	if errors.Is(ctxErr, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err.Error()).WithCause(err)
	} else if errors.Is(ctxErr, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err.Error()).WithCause(err)
	}
	return err
}

// wrapIfLikelyH2CNotConfiguredError adds a wrapping error that has a message
// telling the caller that they likely need to use h2c but are using a raw http.Client{}.
//
// This happens when running a gRPC-only server.
// This is fragile and may break over time, and this should be considered a best-effort.
func wrapIfLikelyH2CNotConfiguredError(request *http.Request, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := asError(err); ok {
		return err
	}
	if url := request.URL; url != nil && url.Scheme != "http" {
		// If the scheme is not http, we definitely do not have an h2c error, so just return.
		return err
	}
	// net/http code has been investigated and there is no typing of any of these errors
	// they are all created with fmt.Errorf
	// grpc-go returns the first error 2/3-3/4 of the time, and the second error 1/4-1/3 of the time
	if errString := err.Error(); strings.HasPrefix(errString, `Post "`) &&
		(strings.Contains(errString, `net/http: HTTP/1.x transport connection broken: malformed HTTP response`) ||
			strings.HasSuffix(errString, `write: broken pipe`)) {
		return fmt.Errorf("possible h2c configuration issue when talking to gRPC server, see %s: %w", commonErrorsURL, err)
	}
	return err
}

// wrapIfLikelyWithGRPCNotUsedError adds a wrapping error that has a message
// telling the caller that they likely forgot to use connecthttp.WithGRPC().
//
// This happens when running a gRPC-only server.
// This is fragile and may break over time, and this should be considered a best-effort.
func wrapIfLikelyWithGRPCNotUsedError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := asError(err); ok {
		return err
	}
	// golang.org/x/net code has been investigated and there is no typing of this error
	// it is created with fmt.Errorf
	// http2/transport.go:573:	return nil, fmt.Errorf("http2: Transport: cannot retry err [%v] after Request.Body was written; define Request.GetBody to avoid this error", err)
	if errString := err.Error(); strings.HasPrefix(errString, `Post "`) &&
		strings.Contains(errString, `http2: Transport: cannot retry err`) &&
		strings.HasSuffix(errString, `after Request.Body was written; define Request.GetBody to avoid this error`) {
		return fmt.Errorf("possible missing connecthttp.WithGRPC() client option when talking to gRPC server, see %s: %w", commonErrorsURL, err)
	}
	return err
}

// HTTP/2 has its own set of error codes, which it sends in RST_STREAM frames.
// When the server sends one of these errors, we should map it back into our
// RPC error codes following
// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#http2-transport-mapping.
//
// This would be vastly simpler if we were using x/net/http2 directly, since
// the StreamError type is exported. When x/net/http2 gets vendored into
// net/http, though, all these types become unexported...so we're left with
// string munging.
func wrapIfRSTError(ctx context.Context, err error) error {
	const (
		streamErrPrefix = "stream error: "
		fromPeerSuffix  = "; received from peer"
	)
	if err == nil {
		return nil
	}
	if _, ok := asError(err); ok {
		return err
	}
	if urlErr := new(url.Error); errors.As(err, &urlErr) {
		// If we get an RST_STREAM error from http.Client.Do, it's wrapped in a
		// *url.Error.
		err = urlErr.Unwrap()
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, streamErrPrefix) {
		return err
	}
	if !strings.HasSuffix(msg, fromPeerSuffix) {
		return err
	}
	msg = strings.TrimSuffix(msg, fromPeerSuffix)
	i := strings.LastIndex(msg, ";")
	if i < 0 || i >= len(msg)-1 {
		return err
	}
	msg = msg[i+1:]
	msg = strings.TrimSpace(msg)
	switch msg {
	case "NO_ERROR", "PROTOCOL_ERROR", "INTERNAL_ERROR", "FLOW_CONTROL_ERROR",
		"SETTINGS_TIMEOUT", "FRAME_SIZE_ERROR", "COMPRESSION_ERROR", "CONNECT_ERROR":
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	case "REFUSED_STREAM":
		return connect.NewError(connect.CodeUnavailable, err.Error()).WithCause(err)
	case "CANCEL":
		if deadline, ok := ctx.Deadline(); ok && time.Now().After(deadline) {
			// Some server implementations will cancel the HTTP/2 stream with
			// a RST_STREAM frame when they observe that the client's deadline
			// has elapsed.
			// We don't inspect ctx.Err() because we could be racing with the
			// timer goroutine that is setting it. But there is no race when
			// directly inspecting the context's deadline. In fact, if we get
			// here, we have likely already examined ctx.Err() in a prior call
			// to wrapIfContextError but observed a nil error and then fell
			// through to here.
			return connect.NewError(connect.CodeDeadlineExceeded, err.Error()).WithCause(err)
		}
		return connect.NewError(connect.CodeCanceled, err.Error()).WithCause(err)
	case "ENHANCE_YOUR_CALM":
		return connect.Errorf(connect.CodeResourceExhausted, "bandwidth exhausted: %v", err).WithCause(err)
	case "INADEQUATE_SECURITY":
		return connect.Errorf(connect.CodePermissionDenied, "transport protocol insecure: %v", err).WithCause(err)
	default:
		return err
	}
}

// wrapIfMaxBytesError wraps errors returned reading from a http.MaxBytesHandler
// whose limit has been exceeded.
func wrapIfMaxBytesError(err error, tmpl string, args ...any) error {
	if err == nil {
		return nil
	}
	if _, ok := asError(err); ok {
		return err
	}
	var maxBytesErr *http.MaxBytesError
	if ok := errors.As(err, &maxBytesErr); !ok {
		return err
	}
	prefix := fmt.Sprintf(tmpl, args...)
	return connect.Errorf(connect.CodeResourceExhausted, "%s: exceeded %d byte http.MaxBytesReader limit", prefix, maxBytesErr.Limit)
}
