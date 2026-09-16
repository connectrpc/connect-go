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

// streamPair wires the client- and server-side halves of a streaming
// in-process RPC together.
//
// Messages flow through unbuffered channels, providing natural backpressure:
// each Send blocks until the peer's Receive consumes the message. The server
// runs on its own goroutine, which is started lazily upon the first call to
// SendHeaders, Send, or Receive.
//
// Lifecycle channels:
//   - closeSendCh: Closed by the client's CloseSend. The server's Receive
//     subsequently returns [io.EOF] once requestCh has drained.
//   - serverDone: Closed by the dispatch goroutine after the server returns.
//     This carries the final server error and signals that response metadata
//     and trailers have safely synced into the client's CallInfo.
//
// Clients read responseCh until [io.EOF] or call Close to cancel the stream.
//
// Note: Unary RPCs bypass this structure to use a synchronous fast path
// (see unary.go).
type streamPair struct {
	t          *transport
	spec       connect.Spec
	clientInfo *connect.CallInfo
	serverInfo *connect.CallInfo

	// ctx is the stream context, derived from the call context. The server
	// runs on it. Close cancels it via cancel.
	ctx    context.Context
	cancel context.CancelFunc

	requestCh   chan any
	responseCh  chan any
	closeSendCh chan struct{}
	serverDone  chan struct{}

	dispatchOnce sync.Once
	serverErr    error

	closeSendOnce sync.Once
}

func newStreamPair(ctx context.Context, t *transport, spec connect.Spec) *streamPair {
	ctx, cancel := context.WithCancel(ctx)
	clientInfo, _ := connect.CallInfoForClientContext(ctx)
	pair := &streamPair{
		t:           t,
		spec:        spec,
		clientInfo:  clientInfo,
		serverInfo:  &connect.CallInfo{Spec: spec},
		ctx:         ctx,
		cancel:      cancel,
		requestCh:   make(chan any),
		responseCh:  make(chan any),
		closeSendCh: make(chan struct{}),
		serverDone:  make(chan struct{}),
	}
	if pair.clientInfo != nil {
		pair.clientInfo.Spec = spec
	}
	return pair
}

// dispatch lazily launches the server goroutine. Safe to call from
// any Send/Receive entry point; subsequent calls are no-ops.
func (p *streamPair) dispatch() {
	p.dispatchOnce.Do(func() {
		if p.clientInfo != nil {
			syncHeader(p.serverInfo.RequestHeader(), p.clientInfo.RequestHeader())
		}
		go p.run()
	})
}

// run executes the server and finalises the stream. Closing responseCh
// signals io.EOF to client.Receive. The defer order matters: metadata sync
// must happen before either channel is closed.
func (p *streamPair) run() {
	defer close(p.serverDone)
	defer close(p.responseCh)
	defer func() {
		if r := recover(); r != nil {
			p.serverErr = connect.Errorf(connect.CodeInternal, "panic in server: %v", r)
		}
		if p.clientInfo != nil {
			syncHeader(p.clientInfo.ResponseHeader(), p.serverInfo.ResponseHeader())
			syncHeader(p.clientInfo.ResponseTrailer(), p.serverInfo.ResponseTrailer())
		}
	}()
	hs := &serverStream{p: p}
	p.serverErr = p.t.server.Call(p.ctx, p.spec.Procedure, p.serverInfo, hs)
}

// clientStream is the client-side half of a streamPair.
type clientStream struct {
	p *streamPair
}

func (s *clientStream) SendHeaders() error {
	s.p.dispatch()
	return nil
}

func (s *clientStream) Send(msg any) error {
	select {
	case <-s.p.closeSendCh:
		return io.EOF
	default:
		s.p.dispatch()
	}
	select {
	case <-s.p.closeSendCh:
		return io.EOF
	case <-s.p.serverDone:
		return io.EOF
	case <-s.p.ctx.Done():
		return s.p.ctx.Err()
	case s.p.requestCh <- msg:
		return nil
	}
}

func (s *clientStream) CloseSend() error {
	s.p.closeSendOnce.Do(func() {
		close(s.p.closeSendCh)
	})
	return nil
}

func (s *clientStream) Receive(dst any) error {
	s.p.dispatch()
	select {
	case <-s.p.ctx.Done():
		return s.p.ctx.Err()
	case msg, ok := <-s.p.responseCh:
		if !ok {
			// Server closed, release the stream context.
			s.p.cancel()
			if s.p.serverErr != nil {
				return asClientErr(s.p.serverErr)
			}
			return io.EOF
		}
		return s.p.t.copy(dst, msg)
	}
}

func (s *clientStream) Close() error {
	err := s.CloseSend()
	s.p.cancel()
	return err
}

// serverStream is the server-side half of a streamPair.
type serverStream struct {
	p *streamPair
}

// SendHeaders is a no-op for the in-process transport.
func (s *serverStream) SendHeaders() error { return nil }

func (s *serverStream) Receive(dst any) error {
	// An aborted RPC is a cancellation of the call context, so ctx.Done
	// reports it. closeSendCh is the clean half-close: report io.EOF once
	// requestCh has drained, draining a request that raced with CloseSend.
	select {
	case <-s.p.ctx.Done():
		return s.p.ctx.Err()
	case msg := <-s.p.requestCh:
		return s.p.t.copy(dst, msg)
	case <-s.p.closeSendCh:
		select {
		case msg := <-s.p.requestCh:
			return s.p.t.copy(dst, msg)
		default:
			return io.EOF
		}
	}
}

func (s *serverStream) Send(msg any) error {
	select {
	case <-s.p.ctx.Done():
		return s.p.ctx.Err()
	case s.p.responseCh <- msg:
		return nil
	}
}

// asClientErr converts a server's returned error into the verdict the
// client observes, mirroring the serialization and deserialization
// of a wire transport.
//
// To accurately reflect the client-side state, it applies the following rules:
//   - Remote errors forwarded from an upstream call are scrubbed to
//     [CodeInternal] to prevent leaking their original code, message,
//     and details as the server's own verdict (see [Error.IsRemote]).
//   - Standard errors (non-[*Error]) are converted to [CodeUnknown],
//     except for context cancellation and deadline expiry, which retain
//     their respective codes.
//   - The resulting error is marked as remote, mimicking a client-side
//     decoded error.
//
// The server's original [*Error] is left untouched so it can be safely
// used for server-side logging.
func asClientErr(err error) error {
	if err == nil {
		return nil
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		if cerr.IsRemote() {
			return connect.NewError(connect.CodeInternal, "").WithCause(err).WithRemote()
		}
		return cerr.WithRemote()
	}
	code := connect.CodeUnknown
	switch {
	case errors.Is(err, context.Canceled):
		code = connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		code = connect.CodeDeadlineExceeded
	}
	return connect.NewError(code, "").WithCause(err).WithRemote()
}

func syncHeader(dst, src *connect.Header) {
	for key, vals := range src.All() {
		dst.SetValues(key, vals)
	}
}
