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

package wstransport

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/connecterr"
	"github.com/coder/websocket"
)

// abandonWriteAfter is how long a write may go on after its RPC ends. One that
// takes longer is stuck on a peer that stopped reading, so the connection goes.
const abandonWriteAfter = time.Second

// peerGoneGrace bounds how long a failed write waits for the reader to notice
// the connection it failed on is gone, which takes microseconds when it is.
const peerGoneGrace = 100 * time.Millisecond

// websocketHandlerConn is the server's view of one RPC. [session.Serve] wraps
// it in a [connect.ServerStream] and hands that to [connect.Server.Call].
type websocketHandlerConn struct {
	frameDecoder

	// ctx is the RPC's, for both directions. Send, SendHeaders and Receive come
	// from the handler, which passes none.
	ctx    context.Context //nolint:containedctx
	wsConn *websocket.Conn

	marshaler messageWriter

	// The read side takes one message per WebSocket frame, handling C and M
	// itself before decoding bodies. No read state carries between frames.
	reader *frameReader
	// keeper is consulted only to say why the connection ended. Nil when
	// keep-alive is off.
	keeper *keepAlive
	// callInfo receives M messages. They arrive after the
	// handshake, so this is the only way a browser's metadata reaches the
	// handler.
	callInfo *connect.CallInfo
	// info is carried so a fault can be attributed to the connection that
	// produced it; its OnProtocolError is the monitor. Its
	// InfrastructureHeaders are what a client may not send in an M message.
	info SessionInfo

	// eof stops the read path; sawEndOfClientStream says why it stopped. A
	// client that sent C finished, and one whose connection died did not —
	// the same condition at the same call site, told apart only by this.
	eof                  bool
	sawEndOfClientStream bool
	peerFaulted          bool
	// textEncodingErr ends the read path for good: §6.1 has invalid UTF-8 fail
	// the connection outright, with 1007 and no S message, as RFC 6455 §8.1
	// requires.
	textEncodingErr *connect.Error
	// discardOnce guards the post-C drain, which must not be started twice.
	discardOnce sync.Once
	// cancelCall ends the RPC when the drain finds a message after C.
	cancelCall context.CancelFunc
	// afterEndOfStreamMu orders the drain goroutine and Close, which both look
	// for a message after C. afterEndOfStream and afterEndOfStreamFault record
	// the first one found, which Close reports.
	afterEndOfStreamMu    sync.Mutex
	afterEndOfStream      *connect.Error
	afterEndOfStreamFault ProtocolFault

	leadingSent bool
	// headerErr is a response header the handler set that §7.3.1 does not let
	// this server send.
	headerErr *connect.Error
	// shuttingDown reports that the HTTP server is shutting down, which §13
	// reports as unavailable and closes with 1001. Nil means it never is.
	shuttingDown func() bool
}

// Receive records whether a failure was the peer's doing, which decides how
// the connection is torn down.
func (c *websocketHandlerConn) Receive(msg any) error {
	c.fault = faultNone
	err := c.unmarshal(c.ctx, msg)
	c.reportFault(err)
	if err != nil {
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

// SendHeaders sends the M message now, before any body (§7.4). Headers set
// after it are late and are not sent.
func (c *websocketHandlerConn) SendHeaders() error {
	if err := c.flushLeadingMetadata(c.ctx); err != nil {
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

func (c *websocketHandlerConn) Send(msg any) error {
	if err := c.flushLeadingMetadata(c.ctx); err != nil {
		return err
	}
	if err := c.marshaler.writeBody(c.ctx, c.info.Codec, msg); err != nil {
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

// terminalWriteError decides whether a failed terminal write is worth
// reporting.
//
// It is not when the peer has already left: the write was always going to
// fail, a sender must not assume its S was read anyway, and reporting it would
// log an error for every subscription a client walks away from.
func (c *websocketHandlerConn) terminalWriteError(marshalErr *connect.Error) *connect.Error {
	if marshalErr != nil && c.peerGone() {
		return nil
	}
	return marshalErr
}

// peerGone reports that the read path already saw this connection end without
// the client's C message. Anything written afterwards is written to nobody.
func (c *websocketHandlerConn) peerGone() bool {
	// Two ways to lose a peer: it never sent C and stopped reading, or it sent
	// C and then went away while the handler was still working. §8 makes no
	// distinction — either way the connection ended before S.
	if c.reader.hasEnded() {
		return true
	}
	if c.eof && !c.sawEndOfClientStream {
		return true
	}
	// The reader runs ahead of the handler and of the post-C drain, so it can
	// see the connection end before either has read the failure.
	return c.reader.connectionEnded(peerGoneGrace)
}

// Close sends the verdict and closes the connection. ctx is the RPC's context,
// and an expired deadline there is often the verdict itself, so the final
// writes run detached from it, bounded by wsCloseWriteTimeout from this call.
func (c *websocketHandlerConn) Close(ctx context.Context, err error) error {
	goingAway, err := c.outcome(err)
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wsCloseWriteTimeout)
	defer cancel()
	// §6.1: invalid UTF-8 fails the connection outright, with no S message.
	if utf8Err := c.textEncodingErr; utf8Err != nil {
		_ = closeWithDrain(
			c.wsConn, c.info.drain, websocket.StatusInvalidFramePayloadData, closeReason(utf8Err.Message()),
		)
		return nil
	}
	// §8: a connection that ended before S means the caller can no longer
	// receive one, so none is attempted. Gated on the reader's own signal, not
	// on peerGone: eof is set by any failed read, including an oversized
	// message this end refused while its sender waits for the verdict.
	if c.reader.hasEnded() {
		_ = c.wsConn.CloseNow()
		return nil
	}
	marshalErr, err := c.writeVerdict(closeCtx, err)
	closeCode, closeMessage := c.closeStatus(err, marshalErr, goingAway)
	// §13: a faulted peer gets the code for its fault too, since a browser
	// script can read the code but not the S message. The drain is capped, so
	// a peer with a bomb queued behind its fault cannot make this read it all.
	_ = closeWithDrain(c.wsConn, c.info.drain, closeCode, closeReason(closeMessage))
	if marshalErr != nil {
		return marshalErr
	}
	return nil
}

// outcome decides what the RPC's result is, given what the handler returned,
// and reports whether the server shutting down produced it.
func (c *websocketHandlerConn) outcome(err error) (goingAway bool, result error) {
	// As over HTTP, only a locally authored *connect.Error reaches the client
	// intact, so a handler cannot leak internal text or relay an upstream's
	// verdict as its own.
	err = connecterr.ScrubHandlerError(err)
	// The shutdown canceled the RPC, so the cancellation is the server's, and
	// the client may retry elsewhere.
	goingAway = err != nil && connect.CodeOf(err) == connect.CodeCanceled &&
		c.shuttingDown != nil && c.shuttingDown()
	if goingAway {
		err = errorf(connect.CodeUnavailable, "server is shutting down")
	}
	// A message after C is the RPC's outcome whatever the handler concluded:
	// the peer broke the framing, and §8 makes that a protocol error. One the
	// reader has already taken counts, even if the drain has not seen it yet.
	if !c.sawEndOfClientStream {
		return goingAway, err
	}
	if afterC, fault := c.checkAfterEndOfStream(); afterC != nil {
		err = afterC
		c.fault = fault
		c.reportFault(afterC)
	}
	return goingAway, err
}

// writeVerdict sends the leading metadata if it is still pending, then the S
// message. It returns the write's failure, and the outcome, which an
// unsendable header can replace.
func (c *websocketHandlerConn) writeVerdict(ctx context.Context, err error) (writeErr *connect.Error, result error) {
	// Leading metadata precedes the S message even when no body was
	// sent, so headers stay headers rather than folding into the trailers.
	marshalErr := c.flushLeadingMetadata(ctx)
	if marshalErr != nil && marshalErr == c.headerErr {
		marshalErr = nil
	}
	// An unsendable header is the outcome unless the handler reported its own.
	if err == nil && c.headerErr != nil {
		err = c.headerErr
	}
	if marshalErr == nil {
		marshalErr = c.marshaler.writeEndStream(ctx, err, c.responseTrailer())
	}
	return c.terminalWriteError(marshalErr), err
}

// closeStatus picks the close code and reason that end the connection.
func (c *websocketHandlerConn) closeStatus(
	err error,
	marshalErr *connect.Error,
	goingAway bool,
) (websocket.StatusCode, string) {
	switch {
	case marshalErr != nil:
		return websocket.StatusInternalError, marshalErr.Message()
	case c.peerFaulted:
		// The S message carries the verdict; the code says which kind of
		// mistake it was to a peer that reads codes and not JSON — which is
		// every browser script.
		var closeMessage string
		if connectErr, ok := errors.AsType[*connect.Error](err); ok {
			closeMessage = connectErr.Message()
		}
		return serverCloseCode(c.fault), closeMessage
	case goingAway:
		return websocket.StatusGoingAway, ""
	}
	return websocket.StatusNormalClosure, ""
}

// reportFault hands a framing fault to the monitor. The dispatch loop,
// drainLeadingMetadata, and Close's check for a message after C all detect
// faults, so all must report through here or one would be silently dropped.
//
// Reporting is gated on the classification, not on peerFault: that predicate
// decides how to tear the connection down, and some framing faults are reported
// to the caller as Internal rather than as the peer's fault. Observation only —
// callers return err unchanged.
func (c *websocketHandlerConn) reportFault(err *connect.Error) {
	if err == nil {
		return
	}
	if c.fault != faultNone || peerFault(err) {
		c.peerFaulted = true
	}
	if c.fault != faultNone && c.info.OnProtocolError != nil {
		c.info.OnProtocolError(c.info, c.fault, err)
	}
}

func (c *websocketHandlerConn) unmarshal(ctx context.Context, message any) *connect.Error {
	if c.textEncodingErr != nil {
		return c.textEncodingErr
	}
	if c.eof {
		return c.endOfStreamError()
	}
	wire, readErr := c.nextMessage(ctx)
	if readErr != nil {
		return readErr
	}
	return c.dispatch(ctx, wire, message)
}

// endOfStreamError says why the request stream stopped: the client finished,
// or it went away.
//
// The cancellation deliberately does not wrap the transport's error. A read on
// a closed connection reports io.EOF at the bottom of its chain, and wrapping
// it would make errors.Is(err, io.EOF) true — the test every handler uses to
// learn that a stream ended cleanly. A dead client would read as a finished
// one, and a handler that commits on end-of-stream would commit a truncated
// stream. The text keeps the detail; the chain must not.
func (c *websocketHandlerConn) endOfStreamError() *connect.Error {
	if c.sawEndOfClientStream {
		return errorf(connect.CodeUnknown, "%w", io.EOF)
	}
	if c.keeper.timedOut() {
		return errorf(
			connect.CodeCanceled,
			"client sent nothing for %v, not even a Pong (keep-alive timeout)", c.keeper.timeout(),
		)
	}
	// Canceled rather than Unavailable: the peer stopped, the transport did
	// not fail, and a caller should not retry on the client's behalf.
	return errorf(connect.CodeCanceled, "websocket closed before end-of-stream")
}

// nextMessage reads one frame and returns the message it carries.
func (c *websocketHandlerConn) nextMessage(ctx context.Context) (wireMessage, *connect.Error) {
	messageType, frame, readerErr := c.reader.next(ctx)
	if readerErr != nil && errors.Is(readerErr, context.Canceled) && c.reader.hasEnded() {
		// The reader canceled the call because the connection ended. What it
		// read before the end is still delivered, in order, and then the end
		// itself, so the handler hears why rather than a bare cancellation.
		messageType, frame, readerErr = c.reader.nextAfterEnd()
	}
	if readerErr != nil {
		c.eof = true
		// The call ended, not the connection: a deadline is deadline_exceeded.
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(readerErr, ctxErr) {
			return wireMessage{}, wsContextError(ctxErr)
		}
		if errors.Is(readerErr, errMessageTooBig) {
			c.fault = FaultSizeLimit
			return wireMessage{}, exceedsReadLimit(c.readMaxBytes)
		}
		if limitErr := peerRejectedAsTooBig(readerErr); limitErr != nil {
			c.fault = FaultSizeLimit
			return wireMessage{}, limitErr
		}
		return wireMessage{}, c.endOfStreamError()
	}
	wire, parseErr := c.parseFrame(messageType, frame)
	if c.fault == FaultTextEncoding {
		c.textEncodingErr = parseErr
	}
	return wire, parseErr
}

// dispatch interprets one message and decodes its payload into message.
func (c *websocketHandlerConn) dispatch(ctx context.Context, wire wireMessage, message any) *connect.Error {
	switch wire.marker {
	case markerMetadata:
		// The opening message was consumed before dispatch; a second one would
		// write CallInfo while the handler is already reading it.
		return c.fail(
			FaultMetadata,
			"client sent a second M message; a stream carries exactly one, and it opens the stream",
		)
	case markerClientEndStream:
		// §6.1: C is a bare control message, always text. The last request
		// message travels in a B before it.
		if !wire.text {
			return c.fail(FaultFrameType, "client sent C in a binary frame; expected a text frame")
		}
		if len(wire.payload) != 0 {
			return c.fail(
				FaultMarker,
				"client sent C with a %d-byte payload; C carries none, so the last message goes in a B",
				len(wire.payload),
			)
		}
		c.eof = true
		c.sawEndOfClientStream = true
		c.discardAfterEndOfStream(ctx)
		return errorf(connect.CodeUnknown, "%w", io.EOF)
	case markerBody:
		if err := c.decodeBody(ctx, c.info.Codec, wire, message); err != nil {
			return err
		}
		if c.callInfo != nil {
			c.callInfo.ReceiveStats = connect.MessageStats{Size: len(wire.payload)}
		}
		return nil
	case markerServerEndStream:
		return c.fail(
			FaultMarker,
			"client sent %s, which only a server may send", markerName(wire.marker),
		)
	default:
		return c.fail(FaultMarker, "client sent unknown marker %s", markerName(wire.marker))
	}
}

// discardAfterEndOfStream reads and throws away whatever the client sends
// after its C message, until the connection closes.
//
// Nothing reads this stream again — Receive returns io.EOF from the flag — so
// without this the socket buffer fills and a peer that keeps sending blocks in
// its own write. Its protocol mistake would become a stall on its side, at the
// point where this end has already decided to ignore it.
//
// Each message is still bounded by the read limit, and the RPC's deadline
// bounds the whole thing, so a peer cannot use this to buy unbounded work. The
// goroutine ends when the context is done or the connection closes, except
// after a protocol error: then it reads until the connection closes.
func (c *websocketHandlerConn) discardAfterEndOfStream(ctx context.Context) {
	c.discardOnce.Do(func() { go c.drainAfterEndOfStream(ctx) })
}

func (c *websocketHandlerConn) drainAfterEndOfStream(ctx context.Context) {
	for c.reader.await(ctx) {
		if afterC, _ := c.checkAfterEndOfStream(); afterC != nil {
			// §5 has a protocol error end the stream: canceling the call stops
			// the handler, and Close sends the S.
			if c.cancelCall != nil {
				c.cancelCall()
			}
			c.readUntilClosed(ctx)
			return
		}
		// The connection ended. The reader has already canceled the call (§8).
		if c.reader.drained() {
			return
		}
	}
}

// readUntilClosed discards messages past the call's cancellation, so the peer
// reaches the verdict rather than stalling on a full buffer.
func (c *websocketHandlerConn) readUntilClosed(ctx context.Context) {
	uncanceled := context.WithoutCancel(ctx)
	for {
		if _, _, err := c.reader.next(uncanceled); err != nil {
			return
		}
	}
}

// checkAfterEndOfStream looks for a message the client sent after C, and
// records the first one as the RPC's outcome (§8). It takes only what has
// already arrived, so Close can call it without waiting.
func (c *websocketHandlerConn) checkAfterEndOfStream() (*connect.Error, ProtocolFault) {
	c.afterEndOfStreamMu.Lock()
	defer c.afterEndOfStreamMu.Unlock()
	if c.afterEndOfStream != nil || c.reader == nil {
		return c.afterEndOfStream, c.afterEndOfStreamFault
	}
	result, ok, _ := c.reader.pop()
	if !ok {
		return nil, faultNone
	}
	switch {
	case result.err == nil:
		c.afterEndOfStream = errorf(
			connect.CodeInternal,
			"protocol error: client sent a message after its C message",
		)
		c.afterEndOfStreamFault = FaultMarker
	case errors.Is(result.err, errMessageTooBig):
		// Still a message after C, and one over the limit, which §12.1 closes
		// with 1009.
		c.afterEndOfStream = errorf(
			connect.CodeResourceExhausted,
			"protocol error: client sent a message after its C message, larger than the %d-byte limit",
			c.readMaxBytes,
		)
		c.afterEndOfStreamFault = FaultSizeLimit
	}
	return c.afterEndOfStream, c.afterEndOfStreamFault
}

// websocketBinarySender is the server's [messageSender]: it writes each
// message as one WebSocket frame rather than appending it to an HTTP response
// body.
type websocketBinarySender struct {
	conn *websocket.Conn
}

func (s *websocketBinarySender) send(ctx context.Context, text bool, data []byte) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Detached, because the library closes the connection when a write's
	// context ends, even mid-write, leaving nowhere to send the S that says why.
	written := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		timer := time.NewTimer(abandonWriteAfter)
		defer timer.Stop()
		select {
		case <-timer.C:
			_ = s.conn.CloseNow()
		case <-written:
		}
	})
	defer stop()
	defer close(written)
	return writeFrame(context.WithoutCancel(ctx), s.conn, text, data)
}

// serverCloseCode is the code §13 assigns each framing fault. A more specific
// one wins over the generic protocol error.
func serverCloseCode(fault ProtocolFault) websocket.StatusCode {
	switch fault {
	case FaultSizeLimit:
		return websocket.StatusMessageTooBig
	case FaultTextEncoding:
		return websocket.StatusInvalidFramePayloadData
	case FaultMarker, FaultFrameType, FaultMetadata, FaultMessageEncoding, faultNone:
		return websocket.StatusProtocolError
	}
	return websocket.StatusProtocolError
}
