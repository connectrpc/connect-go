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
	"sync/atomic"
)

// RecoverInterceptor is an interceptor that recovers from panics. The
// supplied function receives the context and request details.
//
// For streaming RPCs, req.Any() may return nil. It will always be nil
// for client-streaming or bidi-streaming RPCs, since there could be
// zero or even multiple request messages for such RPCs. For
// server-streaming RPCs, it will be nil if the panic occurred before
// the request message was received, which can happen if a panic occurs
// in an interceptor before the RPC handler method is invoked.
//
// Similarly, for streaming RPCs, req.Header() may return nil. This
// could happen in clients when the panic that is recovered occurs
// before the stream is actually created and before request headers are
// even allocated.
//
// Applications will generally want to add this interceptor first, which
// means it will actually be the last to handle any results from the
// RPC handler. This allows for recovering from the panics not only in
// the handler but also in any other interceptors.
//
// The recovered value will never be nil. If panic was called with a nil
// value, the recovered value will be a *[runtime.PanicNilError]. It must
// return an error to send back to the client. If it returns nil, an
// *Error with a code of CodeInternal will ne synthesized. The function
// may also log the panic, emit metrics, or execute other error-handling
// logic. The function must be safe to call concurrently.
//
// By default, handlers don't recover from panics. Because the standard
// library's [http.Server] recovers from panics by default, this option
// isn't usually necessary to prevent crashes. Instead, it helps servers
// collect RPC-specific data during panics and send a more detailed error
// to clients.
//
// Unlike [WithRecover], this interceptor does not do anything special with
// [http.ErrAbortHandler], so the handle function may be called with that as
// the panic value.
//
// Also unlike [WithRecover], which can only be used with handlers, this
// interceptor can be used with clients, to recover from any panics caused
// by bugs in the interceptor chain. For streaming RPCs, this will recover
// from panics that happen in calls to send or receive messages on the
// stream or to close the stream.
func RecoverInterceptor(handle func(ctx context.Context, req AnyRequest, panicValue any) error) Interceptor {
	return &recoverHandlerInterceptor{handle: handle}
}

type recoverHandlerInterceptor struct {
	handle func(context.Context, AnyRequest, any) error
}

func (i *recoverHandlerInterceptor) WrapUnary(next UnaryFunc) UnaryFunc {
	return func(ctx context.Context, req AnyRequest) (_ AnyResponse, retErr error) {
		defer func() {
			if r := recover(); r != nil {
				retErr = i.handle(ctx, req, r)
				if retErr == nil {
					retErr = errorf(CodeInternal, "handler panicked; but recover handler returned non-nil error")
				}
			}
		}()
		return next(ctx, req)
	}
}

func (i *recoverHandlerInterceptor) WrapStreamingHandler(next StreamingHandlerFunc) StreamingHandlerFunc {
	return func(ctx context.Context, conn StreamingHandlerConn) (retErr error) {
		var streamConn *recoverStreamingHandlerConn
		if conn.Spec().StreamType == StreamTypeServer {
			// There will be exactly one request. So we try to capture it
			// so we can provide it to the recover handle func.
			streamConn = &recoverStreamingHandlerConn{StreamingHandlerConn: conn}
			conn = streamConn
		}

		defer func() {
			if panicVal := recover(); panicVal != nil {
				var msg any
				if streamConn != nil {
					if msgPtr := streamConn.req.Load(); msgPtr != nil {
						msg = *msgPtr
					}
				}
				retErr = i.handle(ctx, &recoverStreamRequest{conn, msg}, panicVal)
				if retErr == nil {
					retErr = errorf(CodeInternal, "handler panicked; but recover handler returned non-nil error")
				}
			}
		}()
		return next(ctx, conn)
	}
}

func (i *recoverHandlerInterceptor) WrapStreamingClient(next StreamingClientFunc) StreamingClientFunc {
	return func(ctx context.Context, spec Spec) (conn StreamingClientConn) {
		defer func() {
			if panicVal := recover(); panicVal != nil {
				err := i.handle(ctx, emptyRequest(spec), panicVal)
				if err == nil {
					err = errorf(CodeInternal, "call panicked; but recover handler returned non-nil error")
				}
				conn = &errStreamingClientConn{spec, err}
			}
		}()
		conn = next(ctx, spec)
		return &recoverStreamingClientConn{
			StreamingClientConn: conn,
			ctx:                 ctx,
			handle:              i.handle,
		}
	}
}

type recoverStreamRequest struct {
	StreamingHandlerConn
	msg any
}

func (r *recoverStreamRequest) Any() any {
	return r.msg
}

func (r *recoverStreamRequest) Header() http.Header {
	return r.RequestHeader()
}

func (r *recoverStreamRequest) HTTPMethod() string {
	return http.MethodPost // streams always use POST
}

func (r *recoverStreamRequest) internalOnly() {
}

func (r *recoverStreamRequest) setRequestMethod(_ string) {
	// only invoked internally for unary RPCs; safe to ignore
}

type recoverStreamingHandlerConn struct {
	StreamingHandlerConn
	req atomic.Pointer[any]
}

func (r *recoverStreamingHandlerConn) Receive(msg any) error {
	err := r.StreamingHandlerConn.Receive(msg)
	if err == nil {
		// Note: The framework instantiates msg, passes it to
		// this method, and then returns it to the application.
		// It is possible that the application could mutate the
		// value, so what we provide to the recover handler would
		// then differ from the message actually received. But
		// this is no different than if the RPC handler mutated
		// the request message for a unary RPC and interceptors
		// later examined it via Request.Any. So we tolerate the
		// possibility for server-stream requests, too.
		r.req.Store(&msg)
	}
	return err
}

type emptyRequest Spec

func (e emptyRequest) Any() any {
	return nil
}

func (e emptyRequest) Spec() Spec {
	return Spec(e)
}

func (e emptyRequest) Peer() Peer {
	return Peer{}
}

func (e emptyRequest) Header() http.Header {
	return nil
}

func (e emptyRequest) HTTPMethod() string {
	return http.MethodPost
}

func (e emptyRequest) internalOnly() {
}

func (e emptyRequest) setRequestMethod(_ string) {
	// only invoked internally for unary RPCs; safe to ignore
}

type errStreamingClientConn struct {
	spec Spec
	err  error
}

func (e *errStreamingClientConn) Spec() Spec {
	return e.spec
}

func (e *errStreamingClientConn) Peer() Peer {
	return Peer{}
}

func (e *errStreamingClientConn) Send(_ any) error {
	return e.err
}

func (e *errStreamingClientConn) RequestHeader() http.Header {
	// Clients can add headers before calling Send, so this must be mutable/non-nil.
	return http.Header{} // TODO: memoize so we never allocate more than one?
}

func (e *errStreamingClientConn) CloseRequest() error {
	return e.err
}

func (e *errStreamingClientConn) Receive(_ any) error {
	return e.err
}

func (e *errStreamingClientConn) ResponseHeader() http.Header {
	return nil
}

func (e *errStreamingClientConn) ResponseTrailer() http.Header {
	return nil
}

func (e *errStreamingClientConn) CloseResponse() error {
	return e.err
}

type recoverStreamingClientConn struct {
	StreamingClientConn

	//nolint:containedctx // must memoize the stream context to pass to recover handler
	ctx    context.Context
	handle func(context.Context, AnyRequest, any) error
	req    atomic.Pointer[any]
}

func (r *recoverStreamingClientConn) Send(msg any) error {
	if r.Spec().StreamType == StreamTypeServer {
		// Capture the request message for server-streaming RPCs.
		r.req.Store(&msg)
	}
	return r.invoke(func() error {
		return r.StreamingClientConn.Send(msg)
	})
}

func (r *recoverStreamingClientConn) RequestHeader() http.Header {
	if header := r.StreamingClientConn.RequestHeader(); header != nil {
		return header
	}
	// Clients can add headers before calling Send, so this must be mutable/non-nil.
	// We do this not to recover from a panic but in the hopes of preventing panics in the caller.
	return http.Header{} // TODO: memoize so we never allocate more than one?
}

func (r *recoverStreamingClientConn) CloseRequest() error {
	return r.invoke(r.StreamingClientConn.CloseRequest)
}

func (r *recoverStreamingClientConn) Receive(msg any) error {
	return r.invoke(func() error {
		return r.StreamingClientConn.Receive(msg)
	})
}

func (r *recoverStreamingClientConn) CloseResponse() error {
	return r.invoke(r.StreamingClientConn.CloseResponse)
}

func (r *recoverStreamingClientConn) invoke(action func() error) (retErr error) {
	defer func() {
		if panicVal := recover(); panicVal != nil {
			var msg any
			if msgPtr := r.req.Load(); msgPtr != nil {
				msg = *msgPtr
			}
			retErr = r.handle(r.ctx, &recoverStreamRequest{r, msg}, panicVal)
			if retErr == nil {
				retErr = errorf(CodeInternal, "call panicked; but recover handler returned non-nil error")
			}
		}
	}()
	return action()
}
