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
	"slices"
	"sync"

	"connectrpc.com/connect/v2"
)

// streamPair wires the client and server halves of an in-process RPC
// together. The server runs on its own goroutine, started by the client's
// first SendHeaders, Send, or Receive.
//
// Each direction is a pipe guarded by mu. Receive posts its dst and waits.
// Send copies its message straight into the posted dst. The sender may reuse
// its message once Send returns. Every wait checks cancellation first, then a
// delivered message, then the end of the pipe.
type streamPair struct {
	t          *transport
	spec       connect.Spec
	clientInfo *connect.CallInfo
	serverInfo connect.CallInfo
	server     serverStream

	// ctx is the stream context, derived from the call context. The server
	// runs on it. Close cancels it via cancel.
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	request    pipe
	response   pipe
	started    bool
	stopWake   func() bool // releases the context.AfterFunc that wakes waiters
	headerSent bool
	serverErr  error
}

// dispatch starts the server goroutine once.
func (p *streamPair) dispatch() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return
	}
	p.started = true
	if p.clientInfo != nil {
		syncHeader(p.serverInfo.RequestHeader(), p.clientInfo.RequestHeader())
	}
	p.stopWake = context.AfterFunc(p.ctx, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.request.cond.Broadcast()
		p.response.cond.Broadcast()
	})
	go p.run()
}

// run executes the server and then ends both pipes.
func (p *streamPair) run() {
	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if r := recover(); r != nil {
			p.serverErr = connect.Errorf(connect.CodeInternal, "panic in server: %v", r)
		}
		p.flushHeaders()
		if p.clientInfo != nil {
			syncHeader(p.clientInfo.ResponseTrailer(), p.serverInfo.ResponseTrailer())
		}
		p.request.close()
		p.response.close()
	}()
	p.serverErr = p.t.server.Call(p.ctx, p.spec.Procedure, &p.serverInfo, &p.server)
}

// flushHeaders copies the response headers to the client once, as the
// server's first Send or SendHeaders puts them on the wire. Later changes to
// the server's headers are not sent. The caller must hold mu.
func (p *streamPair) flushHeaders() {
	if p.headerSent {
		return
	}
	p.headerSent = true
	if p.clientInfo != nil {
		syncHeader(p.clientInfo.ResponseHeader(), p.serverInfo.ResponseHeader())
	}
}

// send waits for the receiver to post a dst and copies msg into it. It
// returns io.EOF once the pipe has closed.
func (p *streamPair) send(slot *pipe, msg any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if slot.closed {
			return io.EOF
		}
		if slot.dst != nil {
			break
		}
		slot.cond.Wait()
	}
	dst := slot.dst
	slot.dst = nil
	// Wake the receiver even if the copy panics.
	defer func() {
		slot.filled = true
		slot.cond.Broadcast()
	}()
	slot.err = p.t.copy(dst, msg)
	return nil
}

// receive posts dst and waits for a sender to fill it. It reports whether
// dst was filled. Otherwise err is the context error or io.EOF once the pipe
// has closed.
func (p *streamPair) receive(slot *pipe, dst any) (filled bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	slot.dst = dst
	slot.cond.Broadcast()
	for {
		if slot.filled {
			slot.filled = false
			return true, slot.err
		}
		if err := p.ctx.Err(); err != nil {
			slot.dst = nil
			return false, err
		}
		if slot.closed {
			slot.dst = nil
			return false, io.EOF
		}
		slot.cond.Wait()
	}
}

// pipe hands messages in one direction. The stream contract allows one sender
// and one receiver at a time, so a single slot is enough.
type pipe struct {
	cond   sync.Cond
	dst    any   // posted by a waiting Receive
	filled bool  // set once Send has copied into dst
	err    error // the copy result
	closed bool  // no more messages
}

// close ends the pipe and wakes both sides. The caller must hold mu.
func (pp *pipe) close() {
	pp.closed = true
	pp.cond.Broadcast()
}

// clientStream is the client-side half of a streamPair.
type clientStream struct {
	streamPair

	// end is the result of the Receive that ended the stream. Receive
	// keeps returning it.
	end error
}

func newClientStream(ctx context.Context, t *transport, spec connect.Spec) *clientStream {
	stream := &clientStream{}
	stream.ctx, stream.cancel = context.WithCancel(ctx)
	stream.t = t
	stream.spec = spec
	stream.clientInfo, _ = connect.CallInfoForClientContext(ctx)
	stream.serverInfo.Spec = spec
	stream.server.p = &stream.streamPair
	stream.request.cond.L = &stream.mu
	stream.response.cond.L = &stream.mu
	if stream.clientInfo != nil {
		stream.clientInfo.Spec = spec
	}
	return stream
}

func (s *clientStream) SendHeaders() error {
	s.dispatch()
	return nil
}

func (s *clientStream) Send(msg any) error {
	s.dispatch()
	return s.send(&s.request, msg)
}

func (s *clientStream) CloseSend() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.request.close()
	return nil
}

// Receive reads the single response of a unary or client-streaming RPC and
// then the end of the stream, as connecthttp does.
func (s *clientStream) Receive(dst any) error {
	if s.spec.StreamType != connect.StreamTypeUnary && s.spec.StreamType != connect.StreamTypeClient {
		return s.next(dst)
	}
	if s.end != nil {
		return io.EOF
	}
	err := s.next(dst)
	switch {
	case err == nil:
		if drainErr := s.next(dst); drainErr == nil {
			err = connect.Errorf(connect.CodeUnimplemented, "unary stream has multiple messages")
			s.finish(io.EOF)
		} else if !errors.Is(drainErr, io.EOF) {
			err = drainErr
		}
	case errors.Is(err, io.EOF):
		err = connect.Errorf(connect.CodeUnimplemented, "unary stream has zero messages")
	}
	return err
}

// Close cancels before closing the request side. Waiters woken by the close
// then report the cancellation instead of a clean end.
func (s *clientStream) Close() error {
	s.cancel()
	return s.CloseSend()
}

// next reads the next response. The error that ends the stream is sticky.
func (s *clientStream) next(dst any) error {
	if s.end != nil {
		return s.end
	}
	s.dispatch()
	filled, err := s.receive(&s.response, dst)
	if filled {
		return err
	}
	if errors.Is(err, io.EOF) && s.serverErr != nil {
		err = asClientErr(s.serverErr)
	}
	s.finish(err)
	return err
}

// finish ends the stream with err and releases the stream context.
func (s *clientStream) finish(err error) {
	s.end = err
	s.stopWake()
	s.cancel()
}

// serverStream is the server-side half of a streamPair.
type serverStream struct {
	p *streamPair
}

func (s *serverStream) SendHeaders() error {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	s.p.flushHeaders()
	return nil
}

func (s *serverStream) Receive(dst any) error {
	_, err := s.p.receive(&s.p.request, dst)
	return err
}

func (s *serverStream) Send(msg any) error {
	s.p.mu.Lock()
	s.p.flushHeaders()
	s.p.mu.Unlock()
	return s.p.send(&s.p.response, msg)
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
	if cerr, ok := errors.AsType[*connect.Error](err); ok && cerr != nil {
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

// syncHeader copies src into dst. The values are cloned because the server
// may keep changing src after the copy.
func syncHeader(dst, src *connect.Header) {
	for key, vals := range src.All() {
		dst.SetValues(key, slices.Clone(vals))
	}
}
