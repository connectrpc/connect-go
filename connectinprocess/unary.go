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

package connectinprocess

import (
	"context"
	"errors"
	"io"
	"sync"

	"connectrpc.com/connect/v2"
)

// unaryClientStream is the synchronous fast path for in-process unary RPCs.
//
// Instead of spawning a separate goroutine, the server logic executes
// synchronously during the client's first Receive call. The flow is:
//  1. The client calls Send, capturing the request into a local slot.
//  2. The client calls Receive, which waits for the request and then
//     triggers the server dispatch.
//  3. The server's Receive consumes the captured request slot.
//  4. The server's Send writes the response into a local response slot.
//  5. The client's Receive copies that response into the caller's dst.
//
// Send and Receive may run on separate goroutines. mu guards the handoff.
//
// Streaming RPCs cannot use this layout because the server must run an
// indefinite loop that interleaves with the user's Send and Receive calls
// (see clientStream and serverStream in stream.go).
type unaryClientStream struct {
	t          *transport
	spec       connect.Spec
	ctx        context.Context //nolint:containedctx // the lazy dispatch in Receive runs the server on the call context
	clientInfo *connect.CallInfo
	serverInfo *connect.CallInfo

	mu       sync.Mutex
	sendDone bool          // set by Send, CloseSend, or Close
	closed   bool          // set by Close
	waiter   chan struct{} // set while Receive waits for sendDone

	requestMsg  any
	responseMsg any
	serverErr   error

	sentOnce   bool
	sendClosed bool
	rxEnd      bool
}

func newUnaryClientStream(ctx context.Context, t *transport, spec connect.Spec) *unaryClientStream {
	clientInfo, _ := connect.CallInfoForClientContext(ctx)
	stream := &unaryClientStream{
		t:          t,
		spec:       spec,
		ctx:        ctx,
		clientInfo: clientInfo,
		serverInfo: &connect.CallInfo{Spec: spec},
	}
	if stream.clientInfo != nil {
		stream.clientInfo.Spec = spec
	}
	return stream
}

func (s *unaryClientStream) SendHeaders() error { return nil }

func (s *unaryClientStream) Send(msg any) error {
	if s.sendClosed {
		return io.EOF
	}
	if s.sentOnce {
		return errors.New("connectinprocess: unary stream sent more than once")
	}
	s.sentOnce = true
	s.requestMsg = msg
	// A unary request has one message, so Send completes the send side.
	s.finishSend(false)
	return nil
}

func (s *unaryClientStream) CloseSend() error {
	s.sendClosed = true
	s.finishSend(false)
	return nil
}

func (s *unaryClientStream) Close() error {
	s.finishSend(true)
	return nil
}

func (s *unaryClientStream) Receive(dst any) error {
	if s.rxEnd {
		return io.EOF
	}
	// Every path from here ends the receive side.
	s.rxEnd = true
	if err := s.waitSend(); err != nil {
		return err
	}
	if err := s.dispatch(s.ctx); err != nil {
		return asClientErr(err)
	}
	if s.responseMsg == nil {
		return connect.Errorf(connect.CodeUnimplemented, "unary stream has no message")
	}
	if err := s.t.copy(dst, s.responseMsg); err != nil {
		return err
	}
	s.responseMsg = nil
	return nil
}

// finishSend completes the send side and wakes a waiting Receive.
func (s *unaryClientStream) finishSend(closing bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendDone = true
	s.closed = s.closed || closing
	if s.waiter != nil {
		close(s.waiter)
		s.waiter = nil
	}
}

// waitSend blocks until the send side completes or the stream is closed.
func (s *unaryClientStream) waitSend() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return io.EOF
	}
	if s.sendDone {
		s.mu.Unlock()
		return nil
	}
	waiter := make(chan struct{})
	s.waiter = waiter
	s.mu.Unlock()
	select {
	case <-waiter:
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return context.Canceled
	}
	return nil
}

// dispatch runs the server synchronously. Receive gates it behind rxEnd, so
// it runs exactly once per stream. The server reads the captured request via
// unaryHandlerStream.Receive and writes its response via
// unaryHandlerStream.Send.
func (s *unaryClientStream) dispatch(ctx context.Context) error {
	if s.clientInfo != nil {
		syncHeader(s.serverInfo.RequestHeader(), s.clientInfo.RequestHeader())
	}
	// Recover server panics into a CodeInternal error, matching the
	// streaming path (streamPair.run) and connecthttp. Otherwise the panic
	// would propagate synchronously into the client's calling goroutine.
	func() {
		defer func() {
			if r := recover(); r != nil {
				s.serverErr = connect.Errorf(connect.CodeInternal, "panic in server: %v", r)
			}
		}()
		hs := unaryHandlerStream{s: s}
		s.serverErr = s.t.server.Call(ctx, s.spec.Procedure, s.serverInfo, hs)
	}()
	if s.clientInfo != nil {
		syncHeader(s.clientInfo.ResponseHeader(), s.serverInfo.ResponseHeader())
		syncHeader(s.clientInfo.ResponseTrailer(), s.serverInfo.ResponseTrailer())
	}
	return s.serverErr
}

// unaryHandlerStream is the server-side view used by dispatch.
type unaryHandlerStream struct {
	s *unaryClientStream
}

// SendHeaders is a no-op.
func (h unaryHandlerStream) SendHeaders() error { return nil }

func (h unaryHandlerStream) Receive(dst any) error {
	if h.s.requestMsg == nil {
		return io.EOF
	}
	if err := h.s.t.copy(dst, h.s.requestMsg); err != nil {
		return err
	}
	h.s.requestMsg = nil
	return nil
}

func (h unaryHandlerStream) Send(msg any) error {
	h.s.responseMsg = msg
	return nil
}
