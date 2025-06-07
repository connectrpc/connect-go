// Copyright 2021-2025 The Connect Authors
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
	"net/http"
)

type responseHeaderAddressContextKey struct{}
type responseTrailerAddressContextKey struct{}

func HeaderFromIncomingContext(ctx context.Context) (http.Header, bool) {
	panic("TODO")
}

func HeaderFromOutgoingContext(ctx context.Context) (http.Header, bool) {
	panic("TODO")
}

func WithIncomingHeader(ctx context.Context, header http.Header) context.Context {
	panic("TODO")
}

func WithOutgoingHeader(ctx context.Context, header http.Header) context.Context {
	panic("TODO")
}

// WithGetResponseHeader returns a new context to be given to a client when making a request
// that will result in the header pointer being set to the response header.
func WithGetResponseHeader(ctx context.Context, header *http.Header) context.Context {
	return context.WithValue(ctx, responseHeaderAddressContextKey{}, header)
}

// WithGetResponseTrailer returns a new context to be given to a client when making a request
// that will result in the trailer pointer being set to the response trailer.
func WithGetResponseTrailer(ctx context.Context, trailer *http.Header) context.Context {
	return context.WithValue(ctx, responseTrailerAddressContextKey{}, trailer)
}

// SetResponseHeader sets the response header within a simple handler implementation.
func SetResponseHeader(ctx context.Context, header http.Header) {
	responseHeaderAddress, ok := ctx.Value(responseHeaderAddressContextKey{}).(*http.Header)
	if !ok {
		return
	}
	*responseHeaderAddress = header
}

// SetResponseTrailer sets the response trailer within a simple handler implementation.
func SetResponseTrailer(ctx context.Context, trailer http.Header) {
	responseTrailerAddress, ok := ctx.Value(responseTrailerAddressContextKey{}).(*http.Header)
	if !ok {
		return
	}
	*responseTrailerAddress = trailer
}

func requestFromContext[T any](ctx context.Context, message *T) *Request[T] {
	request := NewRequest[T](message)
	header, ok := HeaderFromOutgoingContext(ctx)
	if ok {
		request.setHeader(header)
	}
	return request
}
