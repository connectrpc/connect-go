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

// Package connecterr holds the error-coding helpers shared by transports and
// by the framing packages they build on.
package connecterr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"connectrpc.com/connect/v2"
)

// AsError uses errors.As to unwrap any error and look for a *connect.Error.
func AsError(err error) (*connect.Error, bool) {
	var connectErr *connect.Error
	ok := errors.As(err, &connectErr)
	return connectErr, ok
}

// WrapIfContextError applies connect.CodeCanceled or
// connect.CodeDeadlineExceeded to Go's context.Canceled and
// context.DeadlineExceeded errors, but only if they haven't already been
// wrapped.
func WrapIfContextError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := AsError(err); ok {
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

// WrapIfContextDone wraps errors with connect.CodeCanceled or
// connect.CodeDeadlineExceeded if the context is done. It leaves
// already-wrapped errors unchanged.
func WrapIfContextDone(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	err = WrapIfContextError(err)
	if _, ok := AsError(err); ok {
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

// WrapIfMaxBytesError wraps errors returned reading from a
// http.MaxBytesHandler whose limit has been exceeded.
func WrapIfMaxBytesError(err error, tmpl string, args ...any) error {
	if err == nil {
		return nil
	}
	if _, ok := AsError(err); ok {
		return err
	}
	var maxBytesErr *http.MaxBytesError
	if ok := errors.As(err, &maxBytesErr); !ok {
		return err
	}
	prefix := fmt.Sprintf(tmpl, args...)
	return connect.Errorf(connect.CodeResourceExhausted, "%s: exceeded %d byte http.MaxBytesReader limit", prefix, maxBytesErr.Limit)
}
