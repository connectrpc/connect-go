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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/connecterr"
	"connectrpc.com/connect/v2/internal/connectwire"
	"github.com/coder/websocket"
)

// Marker framing over WebSocket frames, for both directions.
//
// Each WebSocket frame carries exactly one message: a single marker byte
// naming its kind, then the payload. There is no length prefix — the frame is
// the boundary — and the frame type says how the payload is encoded, binary
// for Protobuf and text for JSON.
//
// Two markers are client-only, because WebSocket lacks two things HTTP
// provides:
//
//   - C (End-Of-Client-Stream) stands in for request-body EOF, which a
//     WebSocket cannot half-close.
//   - M (Leading-Metadata) carries metadata as a JSON array of entries, each a
//     key followed by its values, for browsers, whose WebSocket API cannot set
//     headers on the upgrade. Every stream opens with exactly one, in either
//     direction.
//
// The server answers with zero or more B messages and then one S message,
// whose payload is the EndStreamMessage JSON that Connect's HTTP streaming
// protocol also uses.

const (
	// ProtocolConnectWebSocket identifies the WebSocket transport for the
	// Connect protocol on the [connect.CallInfo].Protocol field.
	ProtocolConnectWebSocket = "connect+ws"

	wsHeaderUpgrade    = "Upgrade"
	wsHeaderConnection = "Connection"
	wsHeaderProtocol   = "Sec-WebSocket-Protocol"
	wsHeaderExtensions = "Sec-WebSocket-Extensions"

	wsQueryTimeoutMs = "connect-timeout-ms"

	// wsCloseWriteTimeout bounds the terminal write, which runs detached from
	// the RPC's deadline so that an expired deadline can still be reported.
	wsCloseWriteTimeout = 5 * time.Second
)

// websocketHandlerConn is the server's view of one RPC. [session.Serve] wraps
// it in a [connect.ServerStream] and hands that to [connect.Server.Call].
type websocketHandlerConn struct {
	// ctx is the RPC's, for both directions. Send, SendHeaders and Receive come
	// from the handler, which passes none.
	ctx    context.Context //nolint:containedctx
	wsConn *websocket.Conn

	marshaler messageWriter
	// unmarshaler also holds the session's SessionInfo and CallInfo, which
	// the conn reads from there rather than keeping copies.
	unmarshaler websocketUnmarshaler

	leadingSent bool
	// headerErr is a response header the handler set that §7.3.1 does not let
	// this server send.
	headerErr *connect.Error
	// shuttingDown reports that the HTTP server is shutting down, which §13
	// reports as unavailable and closes with 1001. Nil means it never is.
	shuttingDown func() bool
}

func (c *websocketHandlerConn) Receive(msg any) error {
	if err := c.unmarshaler.Unmarshal(c.ctx, c.unmarshaler.info.Codec, msg); err != nil {
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

func (c *websocketHandlerConn) Send(msg any) error {
	if err := c.flushLeadingMetadata(c.ctx); err != nil {
		return err
	}
	if err := c.marshaler.writeBody(c.ctx, c.unmarshaler.info.Codec, msg); err != nil {
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

// flushLeadingMetadata writes the server's M message. It must run before the
// first body and before the S message alike: a stream that fails without
// sending a body is where leading metadata is most wanted, and there would be
// nothing else to carry it.
//
// It goes out even when there is no metadata, as []. Every stream opens with
// exactly one M in each direction, so a client can read its first message
// without a branch for the empty case — and can take that message as the
// signal that the server has accepted the stream, which is what the response
// header block gives it for free over HTTP.
func (c *websocketHandlerConn) flushLeadingMetadata(ctx context.Context) *connect.Error {
	if c.leadingSent {
		return nil
	}
	header := make(http.Header)
	// Read at flush time rather than copied in: a handler sets its leading
	// metadata during the call, so no earlier snapshot would be current.
	if callInfo := c.unmarshaler.callInfo; callInfo != nil {
		for key, values := range callInfo.ResponseHeader().All() {
			// §7.3.2: a server never sends a connect- key in its M.
			if isConnectReserved(strings.ToLower(key)) {
				continue
			}
			header[http.CanonicalHeaderKey(key)] = values
		}
	}
	entries, encodeErr := encodeMetadata(header)
	if encodeErr != nil {
		// The handler set a header this server cannot send (§7.3.1). M still
		// opens the stream, empty, and the header becomes the RPC's outcome.
		entries = [][]string{}
		c.headerErr = encodeErr
	}
	// Marked only once written, so that Close still opens with M after a
	// failed Send.
	if err := c.marshaler.writeJSON(ctx, markerMetadata, entries); err != nil {
		return err
	}
	c.leadingSent = true
	return c.headerErr
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
	if c.unmarshaler.reader.hasEnded() {
		return true
	}
	if c.unmarshaler.eof && !c.unmarshaler.sawEndOfClientStream {
		return true
	}
	// The reader runs ahead of the handler and of the post-C drain, so it can
	// see the connection end before either has read the failure.
	return c.unmarshaler.reader.connectionEnded(peerGoneGrace)
}

// abandonWriteAfter is how long a write may go on after its RPC ends. One that
// takes longer is stuck on a peer that stopped reading, so the connection goes.
const abandonWriteAfter = time.Second

// peerGoneGrace bounds how long a failed write waits for the reader to notice
// the connection it failed on is gone, which takes microseconds when it is.
const peerGoneGrace = 100 * time.Millisecond

// peerFault reports whether err says the peer sent something malformed or
// oversized, rather than reporting its own RPC failure.
//
// Only the read path produces these, so a handler's own error never reaches
// it: a service legitimately returning ResourceExhausted still gets a polite
// close. A remote error is the peer reporting its own failure, not malforming
// the stream.
func peerFault(err *connect.Error) bool {
	if err == nil || err.IsRemote() {
		return false
	}
	code := err.Code()
	return code == connect.CodeResourceExhausted || code == connect.CodeInvalidArgument
}

// wsStatusClientProtocolError is the code §13 has a client send for most faults.
// A browser script may only pass 1000 or 3000-4999 to close(), so the reason,
// not the code, says what went wrong.
const wsStatusClientProtocolError = websocket.StatusCode(3102)

// serverCloseCode is the code §13 assigns each framing fault. A more specific
// one wins over the generic protocol error.
func serverCloseCode(fault ProtocolFault) websocket.StatusCode {
	switch fault {
	case FaultFrameType:
		return websocket.StatusUnsupportedData
	case FaultSizeLimit:
		return websocket.StatusMessageTooBig
	case FaultTextEncoding:
		return websocket.StatusInvalidFramePayloadData
	case FaultMarker, FaultMetadata, FaultMessageEncoding, FaultUnknown:
		return websocket.StatusProtocolError
	}
	return websocket.StatusProtocolError
}

// clientCloseCode is the code a client closes with for a server's fault.
// §6.1 requires 1007 for invalid UTF-8, and §12.1 asks a non-browser client,
// which this is, for 1009 on an oversized message. Everything else is 3102.
func clientCloseCode(fault ProtocolFault) websocket.StatusCode {
	switch fault {
	case FaultTextEncoding:
		return websocket.StatusInvalidFramePayloadData
	case FaultSizeLimit:
		return websocket.StatusMessageTooBig
	case FaultUnknown, FaultMarker, FaultFrameType, FaultMetadata, FaultMessageEncoding:
		return wsStatusClientProtocolError
	}
	return wsStatusClientProtocolError
}

// closeClientWithError ends a client's connection with 3102. The code alone
// cannot tell a server what went wrong, so §13 requires the reason.
func closeClientWithError(conn *websocket.Conn, limiter *drainLimiter, reason string) error {
	return closeWithDrain(conn, limiter, wsStatusClientProtocolError, closeReason(reason))
}

// closeReason truncates to the 123 bytes a WebSocket close frame allows: RFC
// 6455 caps a control payload at 125, and the status code takes two of them. An
// over-long reason is not truncated by the WebSocket library but rejected, so
// getting this wrong sends no close frame at all.
func closeReason(reason string) string {
	const maxCloseReasonBytes = 123
	if len(reason) <= maxCloseReasonBytes {
		return reason
	}
	truncated := reason[:maxCloseReasonBytes]
	// A close reason must be valid UTF-8, so back off a cut that landed inside
	// a multi-byte rune. At most three bytes go.
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

// Close sends the verdict and closes the connection. ctx is the RPC's context,
// and an expired deadline there is often the verdict itself, so the final
// writes run detached from it, bounded by wsCloseWriteTimeout from this call.
func (c *websocketHandlerConn) Close(ctx context.Context, err error) error {
	goingAway, err := c.outcome(err)
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wsCloseWriteTimeout)
	defer cancel()
	// §6.1: invalid UTF-8 fails the connection outright, with no S message.
	if utf8Err := c.unmarshaler.textEncodingErr; utf8Err != nil {
		_ = closeWithDrain(
			c.wsConn, c.unmarshaler.info.drain, websocket.StatusInvalidFramePayloadData, closeReason(utf8Err.Message()),
		)
		return nil
	}
	// §8: a connection that ended before S means the caller can no longer
	// receive one, so none is attempted. Gated on the reader's own signal, not
	// on peerGone: eof is set by any failed read, including an oversized
	// message this end refused while its sender waits for the verdict.
	if c.unmarshaler.reader.hasEnded() {
		_ = c.wsConn.CloseNow()
		return nil
	}
	marshalErr, err := c.writeVerdict(closeCtx, err)
	closeCode, closeMessage := c.closeStatus(err, marshalErr, goingAway)
	// §13: a faulted peer gets the code for its fault too, since a browser
	// script can read the code but not the S message. The drain is capped, so
	// a peer with a bomb queued behind its fault cannot make this read it all.
	_ = closeWithDrain(c.wsConn, c.unmarshaler.info.drain, closeCode, closeReason(closeMessage))
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
	if !c.unmarshaler.sawEndOfClientStream {
		return goingAway, err
	}
	if afterC, fault := c.unmarshaler.checkAfterEndOfStream(); afterC != nil {
		err = afterC
		c.unmarshaler.peerFaulted = true
		c.unmarshaler.fault = fault
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

// responseTrailer is what the handler set as trailing metadata. It is read here
// because Close runs once the handler has returned, when it is final.
func (c *websocketHandlerConn) responseTrailer() http.Header {
	trailer := make(http.Header)
	if callInfo := c.unmarshaler.callInfo; callInfo != nil {
		copyToHTTPHeader(trailer, callInfo.ResponseTrailer())
	}
	return trailer
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
	case c.unmarshaler.peerFaulted:
		// The S message carries the verdict; the code says which kind of
		// mistake it was to a peer that reads codes and not JSON — which is
		// every browser script.
		var closeMessage string
		if connectErr, ok := errors.AsType[*connect.Error](err); ok {
			closeMessage = connectErr.Message()
		}
		return serverCloseCode(c.unmarshaler.fault), closeMessage
	case goingAway:
		return websocket.StatusGoingAway, ""
	}
	return websocket.StatusNormalClosure, ""
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

// writeFrame sends one already-encoded message as a single WebSocket frame.
//
// The caller assembles marker and payload into one buffer rather than writing
// them separately, because the library decides whether to compress from the
// size of the *first* write: offering the marker alone would fall under any
// threshold and silently disable compression for every message.
func writeFrame(
	ctx context.Context,
	conn *websocket.Conn,
	text bool,
	data []byte,
) (int64, error) {
	messageType := websocket.MessageBinary
	if text {
		messageType = websocket.MessageText
	}
	return int64(len(data)), conn.Write(ctx, messageType, data)
}

// websocketUnmarshaler reads one message per WebSocket frame and transparently
// handles C (End-Of-Client-Stream) and M (Leading-Metadata) before decoding
// bodies.
//
// No read state carries between frames: the frame is the message boundary.
type websocketUnmarshaler struct {
	frameDecoder

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
}

// Unmarshal records whether a failure was the peer's doing, which decides how
// the connection is torn down.
func (u *websocketUnmarshaler) Unmarshal(ctx context.Context, codec connect.Codec, message any) *connect.Error {
	u.fault = FaultUnknown
	err := u.unmarshal(ctx, codec, message)
	u.reportFault(err)
	return err
}

// reportFault hands a framing fault to the monitor. Both the dispatch loop and
// drainLeadingMetadata detect faults, so both must report through here or a
// fault found during the drain would be silently dropped.
//
// Reporting is gated on the classification, not on peerFault: that predicate
// decides how to tear the connection down, and some framing faults are reported
// to the caller as Internal rather than as the peer's fault. Observation only —
// callers return err unchanged.
func (u *websocketUnmarshaler) reportFault(err *connect.Error) {
	if err == nil {
		return
	}
	if u.fault != FaultUnknown || peerFault(err) {
		u.peerFaulted = true
	}
	if u.fault != FaultUnknown && u.info.OnProtocolError != nil {
		u.info.OnProtocolError(u.info, u.fault, err)
	}
}

func (u *websocketUnmarshaler) unmarshal(ctx context.Context, codec connect.Codec, message any) *connect.Error {
	if u.textEncodingErr != nil {
		return u.textEncodingErr
	}
	if u.eof {
		return u.endOfStreamError()
	}
	wire, readErr := u.nextMessage(ctx)
	if readErr != nil {
		return readErr
	}
	return u.dispatch(ctx, codec, wire, message)
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
func (u *websocketUnmarshaler) endOfStreamError() *connect.Error {
	if u.sawEndOfClientStream {
		return errorf(connect.CodeUnknown, "%w", io.EOF)
	}
	if u.keeper.timedOut() {
		return errorf(
			connect.CodeCanceled,
			"client sent nothing for %v, not even a Pong (keep-alive timeout)", u.keeper.timeout(),
		)
	}
	// Canceled rather than Unavailable: the peer stopped, the transport did
	// not fail, and a caller should not retry on the client's behalf.
	return errorf(connect.CodeCanceled, "websocket closed before end-of-stream")
}

// nextMessage reads one frame and returns the message it carries.
func (u *websocketUnmarshaler) nextMessage(ctx context.Context) (wireMessage, *connect.Error) {
	messageType, frame, readerErr := u.reader.next(ctx)
	if readerErr != nil && errors.Is(readerErr, context.Canceled) && u.reader.hasEnded() {
		// The reader canceled the call because the connection ended. What it
		// read before the end is still delivered, in order, and then the end
		// itself, so the handler hears why rather than a bare cancellation.
		messageType, frame, readerErr = u.reader.nextAfterEnd()
	}
	if readerErr != nil {
		u.eof = true
		// The call ended, not the connection: a deadline is deadline_exceeded.
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(readerErr, ctxErr) {
			return wireMessage{}, wsContextError(ctxErr)
		}
		if errors.Is(readerErr, errMessageTooBig) {
			u.fault = FaultSizeLimit
			return wireMessage{}, exceedsReadLimit(u.readMaxBytes)
		}
		if limitErr := peerRejectedAsTooBig(readerErr); limitErr != nil {
			u.fault = FaultSizeLimit
			return wireMessage{}, limitErr
		}
		return wireMessage{}, u.endOfStreamError()
	}
	wire, parseErr := u.parseFrame(messageType, frame)
	if u.fault == FaultTextEncoding {
		u.textEncodingErr = parseErr
	}
	return wire, parseErr
}

// dispatch interprets one message and decodes its payload into message.
func (u *websocketUnmarshaler) dispatch(
	ctx context.Context,
	codec connect.Codec,
	wire wireMessage,
	message any,
) *connect.Error {
	switch wire.marker {
	case markerMetadata:
		// The opening message was consumed before dispatch; a second one would
		// write CallInfo while the handler is already reading it.
		return u.fail(
			FaultMetadata,
			"client sent a second M message; a stream carries exactly one, and it opens the stream",
		)
	case markerClientEndStream:
		// §6.1: C is a bare control message, always text. The last request
		// message travels in a B before it.
		if !wire.text {
			return u.fail(FaultFrameType, "client sent C in a binary frame; expected a text frame")
		}
		if len(wire.payload) != 0 {
			return u.fail(
				FaultMarker,
				"client sent C with a %d-byte payload; C carries none, so the last message goes in a B",
				len(wire.payload),
			)
		}
		u.eof = true
		u.sawEndOfClientStream = true
		u.discardAfterEndOfStream(ctx)
		return errorf(connect.CodeUnknown, "%w", io.EOF)
	case markerBody:
		if err := u.decodeBody(ctx, codec, wire, message); err != nil {
			return err
		}
		if u.callInfo != nil {
			u.callInfo.ReceiveStats = connect.MessageStats{Size: len(wire.payload)}
		}
		return nil
	case markerServerEndStream:
		return u.fail(
			FaultMarker,
			"client sent %s, which only a server may send", markerName(wire.marker),
		)
	default:
		return u.fail(FaultMarker, "client sent unknown marker %s", markerName(wire.marker))
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
func (u *websocketUnmarshaler) discardAfterEndOfStream(ctx context.Context) {
	u.discardOnce.Do(func() { go u.drainAfterEndOfStream(ctx) })
}

func (u *websocketUnmarshaler) drainAfterEndOfStream(ctx context.Context) {
	for u.reader.await(ctx) {
		if afterC, _ := u.checkAfterEndOfStream(); afterC != nil {
			// §5 has a protocol error end the stream: canceling the call stops
			// the handler, and Close sends the S.
			if u.cancelCall != nil {
				u.cancelCall()
			}
			u.readUntilClosed(ctx)
			return
		}
		// The connection ended. The reader has already canceled the call (§8).
		if u.reader.drained() {
			return
		}
	}
}

// readUntilClosed discards messages past the call's cancellation, so the peer
// reaches the verdict rather than stalling on a full buffer.
func (u *websocketUnmarshaler) readUntilClosed(ctx context.Context) {
	uncanceled := context.WithoutCancel(ctx)
	for {
		if _, _, err := u.reader.next(uncanceled); err != nil {
			return
		}
	}
}

// checkAfterEndOfStream looks for a message the client sent after C, and
// records the first one as the RPC's outcome (§8). It takes only what has
// already arrived, so Close can call it without waiting.
func (u *websocketUnmarshaler) checkAfterEndOfStream() (*connect.Error, ProtocolFault) {
	u.afterEndOfStreamMu.Lock()
	defer u.afterEndOfStreamMu.Unlock()
	if u.afterEndOfStream != nil || u.reader == nil {
		return u.afterEndOfStream, u.afterEndOfStreamFault
	}
	result, ok, _ := u.reader.pop()
	if !ok {
		return nil, FaultUnknown
	}
	switch {
	case result.err == nil:
		u.afterEndOfStream = errorf(
			connect.CodeInternal,
			"protocol error: client sent a message after its C message",
		)
		u.afterEndOfStreamFault = FaultMarker
	case errors.Is(result.err, errMessageTooBig):
		// Still a message after C, and one over the limit, which §12.1 closes
		// with 1009.
		u.afterEndOfStream = errorf(
			connect.CodeResourceExhausted,
			"protocol error: client sent a message after its C message, larger than the %d-byte limit",
			u.readMaxBytes,
		)
		u.afterEndOfStreamFault = FaultSizeLimit
	}
	return u.afterEndOfStream, u.afterEndOfStreamFault
}

// drainLeadingMetadata consumes the single M message that opens
// every stream, so CallInfo is complete and frozen before the handler exists.
// Without it the merge happens on the read path, under the handler, and
// anything the handler does concurrently with its first Receive races that
// write.
//
// Exactly one message, not one-or-more: a drain that kept reading while frames
// were metadata could only discover the end by blocking for a frame the client
// may never send. A receive-only client sends its opening metadata and then
// waits for the server, so the drain must know it is done after one frame.
func (u *websocketUnmarshaler) drainLeadingMetadata(ctx context.Context) *connect.Error {
	u.fault = FaultUnknown
	wire, readErr := u.nextOpeningMessage(ctx)
	if readErr != nil {
		u.reportFault(readErr)
		return readErr
	}
	if wire.marker != markerMetadata {
		// Anything before the opening metadata means the peer is not speaking
		// this protocol; there is no partial state worth keeping.
		failure := u.fail(
			FaultMetadata,
			"client's first message must be M; got %s", markerName(wire.marker),
		)
		u.reportFault(failure)
		return failure
	}
	// §6.1: M is always JSON, and a binary frame says its payload is not.
	if !wire.text {
		failure := u.fail(FaultFrameType, "client sent M in a binary frame; expected a text frame")
		u.reportFault(failure)
		return failure
	}
	mergeErr := u.mergeLeadingMetadata(wire.payload)
	if mergeErr != nil {
		u.reportFault(mergeErr)
	}
	return mergeErr
}

// nextOpeningMessage reads the client's first message, which must arrive
// within the handshake timeout rather than the RPC's much longer deadline.
func (u *websocketUnmarshaler) nextOpeningMessage(callCtx context.Context) (wireMessage, *connect.Error) {
	timeout := u.info.HandshakeTimeout
	if timeout <= 0 {
		return u.nextMessage(callCtx)
	}
	openingCtx, cancel := context.WithTimeoutCause(callCtx, timeout, errNoOpeningMessage)
	defer cancel()
	wire, readErr := u.nextMessage(openingCtx)
	if readErr != nil && callCtx.Err() == nil && errors.Is(context.Cause(openingCtx), errNoOpeningMessage) {
		return wire, errorf(
			connect.CodeDeadlineExceeded, "client sent no opening M message within %v", timeout,
		)
	}
	return wire, readErr
}

func (u *websocketUnmarshaler) mergeLeadingMetadata(payload []byte) *connect.Error {
	entries, parseErr := u.metadataEntries(payload)
	if parseErr != nil {
		return parseErr
	}
	// Checked against the keys as sent, before canonicalization, so the error
	// names the spelling the client used rather than an internal form of it.
	// A reserved key ends the RPC rather than being dropped: a client that
	// believed it had set an identity header would otherwise never learn that
	// the server does not have it.
	for _, entry := range entries {
		if len(entry) == 0 {
			continue // decodeMetadata rejects it
		}
		if reason, reserved := reservedHeaderReason(entry[0], u.info.InfrastructureHeaders); reserved {
			return u.fail(
				FaultMetadata,
				"client set %q in its M message, which is %s", entry[0], reason,
			)
		}
	}
	meta, decodeErr := u.metadataHeader(entries)
	if decodeErr != nil {
		return decodeErr
	}
	// An M key replaces the upgrade request's value, producing the effective
	// headers the handler and its interceptors see. What the connection itself
	// established is protected by the reserved list above, not by precedence.
	header := u.callInfo.RequestHeader()
	for key, values := range meta {
		header.SetValues(key, values)
	}
	return nil
}

// Helpers.

// messageReadLimit turns a payload limit into a wire limit by allowing for the
// marker that precedes it, which §12 leaves out of the limit.
//
// The bound is per WebSocket message, not per frame: a message may be
// fragmented across continuation frames, and both this package's reader and
// coder's SetReadLimit count the reassembled whole. A per-frame bound would be
// defeated by fragmenting.
//
// Zero is never passed through: coder's default is 32KiB, and a zero would cap
// messages at a single byte. Unlimited is -1.
func messageReadLimit(readMaxBytes int) int64 {
	if readMaxBytes <= 0 {
		return -1
	}
	return int64(readMaxBytes) + 1
}

// errMessageTooBig reports a message that overran the limit this side
// enforces. The reader knows the wire limit; only the caller knows the
// configured one worth naming, so the sentinel carries no number.
var errMessageTooBig = errors.New("message exceeds read limit")

// errNoOpeningMessage is the cause when a client's opening M does not arrive
// within the handshake timeout.
var errNoOpeningMessage = errors.New("no opening M message")

// readBoundedMessage reads one WebSocket message, abandoning it as soon as it
// passes limit rather than consuming it. Stopping early is what leaves the
// connection usable enough to tell the peer why it was rejected; coder's own
// SetReadLimit closes with 1009 before this layer can say anything.
//
// The reader spans continuation frames, so the bound is on the reassembled
// message and fragmenting does not evade it. A limit below zero means
// unlimited.
func readBoundedMessage(
	ctx context.Context,
	conn *websocket.Conn,
	limit int64,
	wire *drainLimiter,
) (websocket.MessageType, []byte, error) {
	messageType, reader, err := conn.Reader(ctx)
	if err != nil {
		return 0, nil, err
	}
	if limit < 0 {
		data, err := io.ReadAll(reader)
		return messageType, data, err
	}
	// §12: the compressed bytes are bounded too. Counting starts once the
	// message has begun, so the control frames of an idle connection do not
	// count against the next message.
	wire.beginMessage(limit)
	defer wire.endMessage()
	// One byte past the limit is all it takes to know the message overran, and
	// reading more is what the limit exists to prevent.
	var message bytes.Buffer
	switch _, err := io.CopyN(&message, reader, limit+1); {
	case err == nil:
		return messageType, nil, errMessageTooBig
	case errors.Is(err, io.EOF):
		return messageType, message.Bytes(), nil
	case errors.Is(err, errWireLimit):
		return messageType, nil, errMessageTooBig
	default:
		return messageType, nil, err
	}
}

// peerRejectedAsTooBig recognizes a peer that closed the connection because a
// message from this side blew its read limit. The peer's limit is its own
// business, so the close reason is the only account of it there is.
func peerRejectedAsTooBig(err error) *connect.Error {
	if websocket.CloseStatus(err) == websocket.StatusMessageTooBig {
		return errorf(connect.CodeResourceExhausted, "peer rejected message as too big: %w", err)
	}
	return nil
}

// exceedsReadLimit reports a message abandoned before its size was known, so
// it names the configured limit rather than a size it never measured. The
// limit on the wire carries a marker margin that no caller configured.
func exceedsReadLimit(readMaxBytes int) *connect.Error {
	return errorf(
		connect.CodeResourceExhausted,
		"message exceeds the configured max of %d bytes", readMaxBytes,
	)
}

func isCleanWebSocketClose(err error) bool {
	// Not a switch: the repo's exhaustive linter wants every StatusCode listed,
	// and enumerating thirteen irrelevant close codes would obscure the rule.
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}

// The client mirrors the server: one message per frame, with [messageWriter]
// marshaling outbound and websocketClientUnmarshaler reading inbound and
// recognizing the server's S message.

// wsClientCall owns the lazy dial and is the [messageSender] the
// writer sends through. Dialing on first use rather than at construction is
// what lets a stream be created without a round trip.
type wsClientCall struct {
	ctx context.Context //nolint:containedctx
	// transport supplies the settings every call shares: the codecs and the
	// subprotocols offered for them, the read limit, and the timeouts.
	transport   *clientTransport
	dialOptions *websocket.DialOptions
	url         *url.URL
	// codec starts as the send codec and becomes the server's choice once the
	// handshake completes.
	codec connect.Codec
	// drain caps what the closing handshake reads; see drainLimiter.
	drain *drainLimiter

	// heartbeat carries the server's PINGs and PONGs to reader; the dial
	// options' callbacks hold it.
	heartbeat *heartbeat

	// stopCloseOnDone cancels the Close frame sent when ctx ends, once the
	// connection is being closed anyway.
	stopCloseOnDone func() bool

	// dialOnce also makes concurrent callers wait for the dial in progress.
	dialOnce sync.Once
	wsConn   *websocket.Conn
	reader   *frameReader
	keeper   *keepAlive
	response *http.Response
	dialErr  *connect.Error
}

func (c *wsClientCall) ensureDialed() *connect.Error {
	c.dialOnce.Do(func() {
		conn, err := c.handshake()
		if err != nil {
			c.dialErr = err
			return
		}
		c.start(conn)
	})
	return c.dialErr
}

// handshake dials and checks what the server agreed to. A connection it
// rejects is closed.
func (c *wsClientCall) handshake() (*websocket.Conn, *connect.Error) {
	// The handshake gets its own deadline; the connection outlives it.
	dialCtx := c.ctx
	if c.transport.opts.handshakeTimeout > 0 {
		var cancelDial context.CancelFunc
		dialCtx, cancelDial = context.WithTimeout(c.ctx, c.transport.opts.handshakeTimeout)
		defer cancelDial()
	}
	conn, response, err := websocket.Dial(dialCtx, c.url.String(), c.dialOptions)
	c.response = response
	var failedBody []byte
	if response != nil && response.Body != nil {
		// We only need the upgrade response's headers; close the body to
		// avoid leaking it. On a failed handshake the library has replaced
		// it with the first KiB, which may hold a Connect error.
		if err != nil {
			failedBody, _ = io.ReadAll(response.Body)
		}
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, dialError(err, response, failedBody)
	}
	// The library has already failed a handshake that echoed a token this
	// client did not offer (RFC 6455 §4.1). §4.3 fails one that echoed none
	// the same way: the handshake failed, so no Close frame is sent.
	token := c.selectedSubprotocol(conn.Subprotocol())
	if token == "" {
		_ = conn.CloseNow()
		return nil, errorf(
			connect.CodeInternal,
			"server echoed no Sec-WebSocket-Protocol (offered %s)",
			strings.Join(c.transport.offered, ", "),
		)
	}
	// Every offered token names a codec this client holds.
	if codec, ok := c.transport.codecs.get(codecForSubprotocol(token)); ok {
		c.codec = codec
	}
	if takeoverErr := checkNoContextTakeover(response); takeoverErr != nil {
		_ = closeClientWithError(
			conn, c.drain,
			"permessage-deflate requires client_no_context_takeover and server_no_context_takeover",
		)
		return nil, takeoverErr
	}
	return conn, nil
}

// start begins reading and keeping alive an accepted connection.
func (c *wsClientCall) start(conn *websocket.Conn) {
	// The limit is enforced in readBoundedMessage, as on the server, so an
	// oversized message is classified and closed like any other fault.
	conn.SetReadLimit(-1)
	c.wsConn = conn
	// The library tears a connection down without a Close frame when a
	// read's or write's context ends, so the connection runs detached from
	// the call. §11.1 and §13 have a client whose call ends before S close
	// with 1000, which the server takes as the cancellation (§8).
	readLimit := messageReadLimit(c.transport.opts.readMaxBytes)
	connCtx := context.WithoutCancel(c.ctx)
	c.stopCloseOnDone = context.AfterFunc(c.ctx, func() {
		_ = closeWithDrain(conn, c.drain, websocket.StatusNormalClosure, "")
	})
	c.reader = newFrameReader(
		connCtx,
		func(readCtx context.Context) (websocket.MessageType, []byte, error) {
			return readBoundedMessage(readCtx, conn, readLimit, c.drain)
		},
		nil,
	)
	c.heartbeat.attach(c.reader)
	c.keeper = startKeepAlive(
		connCtx, conn, c.reader, c.transport.opts.keepAliveInterval, c.transport.opts.keepAliveTimeout,
		wsStatusClientProtocolError,
	)
}

// preventDial makes sure this call never dials, and reports whether it already
// had. A dial in progress is waited for.
func (c *wsClientCall) preventDial() bool {
	c.dialOnce.Do(func() {
		c.dialErr = errorf(connect.CodeCanceled, "stream closed before it was used")
	})
	return c.wsConn != nil
}

// selectedSubprotocol maps the echoed token back to the one this client
// offered, or "" when the server echoed none. Token matching is
// case-insensitive, as the library's own check is.
func (c *wsClientCall) selectedSubprotocol(echoed string) string {
	for _, token := range c.transport.offered {
		if echoed != "" && strings.EqualFold(token, echoed) {
			return token
		}
	}
	return ""
}

// send implements [messageSender], writing one message as a single WebSocket
// frame.
func (c *wsClientCall) send(ctx context.Context, text bool, data []byte) (int64, error) {
	if err := c.ensureDialed(); err != nil {
		return 0, err
	}
	// Checked here and the write detached: a write under a context that ends
	// would drop the connection without the Close frame §13 asks for.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	written, err := writeFrame(context.WithoutCancel(ctx), c.wsConn, text, data)
	if err != nil && ctx.Err() == nil {
		// A server may end the RPC before reading every request, so a failed
		// write is how this end learns the stream is over. As over HTTP, it
		// reads as io.EOF and the caller calls Receive for the server's verdict.
		return written, fmt.Errorf("%w: %w", io.EOF, err)
	}
	return written, err
}

type websocketClientConn struct {
	call *wsClientCall
	// localFault records the first failure this end produced rather than read
	// off the wire; its message is the close reason.
	localFault atomic.Pointer[connect.Error]

	marshaler messageWriter
	// openOnce writes the M message that must precede every other message on
	// the stream.
	openOnce sync.Once
	openErr  *connect.Error
	// callInfo supplies the request headers when M is written, and learns the
	// negotiated codec once the handshake completes. Nil when the caller
	// supplied none.
	callInfo    *connect.CallInfo
	unmarshaler websocketClientUnmarshaler

	// responseHeader and responseTrailer are guarded by the unmarshaler's
	// metadataMu, since Close may read them while Receive writes them.
	responseHeader  http.Header
	responseTrailer http.Header

	responseHeaderOnce sync.Once
	// closeResponseOnce makes CloseResponse safe to call from Close and from
	// the release that follows S.
	closeResponseOnce sync.Once
	closeResponseErr  error
	releaseOnce       sync.Once
	sendCloseOnce     sync.Once
	sendCloseErr      *connect.Error
	// Read by Send, which the contract allows to run concurrently with the
	// CloseSend that sets it.
	sendClosed atomic.Bool
}

// open writes the M message that starts the stream. Every message the client
// sends must follow it, so it runs before the first Send and before
// CloseRequest. It is written even when there is no metadata: the server reads
// it to know the stream has begun, and waits for nothing else.
func (c *websocketClientConn) open() *connect.Error {
	c.openOnce.Do(func() {
		// §6.2: once the server's S has arrived, the client sends nothing more,
		// not even the M it had not yet sent.
		if c.unmarshaler.endStreamSeen.Load() {
			return
		}
		// Checked before anything is written, so a header this client may not
		// send fails the call here rather than as the server's protocol error.
		requestMeta := c.requestMetadata()
		if metadataErr := checkRequestMetadata(requestMeta); metadataErr != nil {
			c.openErr = metadataErr
			return
		}
		entries, encodeErr := encodeMetadata(requestMeta)
		if encodeErr != nil {
			c.openErr = encodeErr
			return
		}
		c.openErr = c.marshaler.writeJSON(c.call.ctx, markerMetadata, entries)
		// Writing M is what dials, so the server's codec choice is known now.
		// A failed dial leaves nothing to report.
		if c.call.wsConn == nil {
			return
		}
		if c.callInfo != nil {
			c.callInfo.Codec = c.call.codec.Name()
		}
	})
	return c.openErr
}

// requestMetadata reads the caller's request headers as they stand when M is
// written, which is the first Send as it is over HTTP.
//
// They travel in M and nowhere else. Copying them onto the handshake too would
// put the same keys on two channels, and the client's own M would be shadowed
// by its own headers the moment the two disagreed.
func (c *websocketClientConn) requestMetadata() http.Header {
	header := make(http.Header, 8)
	if c.callInfo == nil {
		return header
	}
	for key, values := range c.callInfo.RequestHeader().All() {
		header[http.CanonicalHeaderKey(key)] = append(header[http.CanonicalHeaderKey(key)], values...)
	}
	return header
}

func (c *websocketClientConn) Send(msg any) error {
	// The peer stops reading bodies once it has seen end-of-client-stream, so
	// this message would be discarded. connecthttp reports the same
	// mistake rather than letting the caller believe it was delivered.
	if c.sendClosed.Load() {
		return errorf(connect.CodeUnknown, "send after CloseSend: %w", io.EOF)
	}
	// Writing now would race this end's reply to the server's Close frame,
	// and the server discards anything sent after its S.
	if c.unmarshaler.endStreamSeen.Load() {
		return errorf(connect.CodeUnknown, "send after the server ended the stream: %w", io.EOF)
	}
	if err := c.open(); err != nil {
		return err
	}
	if err := c.marshaler.writeBody(c.call.ctx, c.call.codec, msg); err != nil {
		c.noteLocalFault(err)
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

func (c *websocketClientConn) CloseRequest() error {
	// §8: no C once the response stream has ended. Writing one would race
	// this end's reply to the server's Close frame.
	if c.unmarshaler.endStreamSeen.Load() {
		c.sendClosed.Store(true)
		return nil
	}
	// A stream that sent no message still has to open before it can end.
	if err := c.open(); err != nil {
		if c.requestSideEnded(err) {
			c.sendClosed.Store(true)
			return nil
		}
		return err
	}
	c.sendCloseOnce.Do(func() {
		defer c.sendClosed.Store(true)
		// §6.1: C is a bare control message and always text, whatever the
		// codec, so an empty final message is never mistaken for it.
		c.sendCloseErr = c.marshaler.send(c.call.ctx, markerClientEndStream, true, nil)
	})
	if c.sendCloseErr != nil && !c.requestSideEnded(c.sendCloseErr) {
		return c.sendCloseErr
	}
	return nil
}

// requestSideEnded reports a failed write that leaves nothing for CloseRequest
// to close: the server ended the stream, or the call's context did. Receive
// reports why, as it does when connecthttp's CloseSend succeeds in either case.
func (c *websocketClientConn) requestSideEnded(err error) bool {
	return errors.Is(err, io.EOF) || c.call.ctx.Err() != nil
}

func (c *websocketClientConn) Receive(msg any) error {
	// A stream that only receives still has to open: the server will not
	// dispatch the handler until the opening metadata arrives. If the server
	// has already ended the stream, its verdict is still there to read.
	if err := c.open(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	err := c.unmarshaler.Unmarshal(msg)
	if err == nil {
		return nil
	}
	// The trailers go on the conn, which the transport publishes to CallInfo,
	// rather than on the error.
	if c.unmarshaler.endStreamSeen.Load() {
		c.unmarshaler.metadataMu.Lock()
		mergeHeaders(c.responseTrailer, c.unmarshaler.trailer)
		c.unmarshaler.metadataMu.Unlock()
		if c.unmarshaler.endStreamReceived.Load() {
			c.releaseAfterEndOfStream()
		}
		if serverErr := c.unmarshaler.endStreamError; serverErr != nil {
			return serverErr
		}
	}
	return err
}

// releaseAfterEndOfStream closes the connection once S has arrived. A caller
// that reads to io.EOF need not call Close, so nothing else would release it
// from a server that does not close first (§9.1 is only a SHOULD).
func (c *websocketClientConn) releaseAfterEndOfStream() {
	c.releaseOnce.Do(func() {
		go func() { _ = c.CloseResponse() }()
	})
}

// ResponseHeader returns a copy of the response headers, safe to read while
// Receive runs.
func (c *websocketClientConn) ResponseHeader() http.Header {
	// Dialed before locking: the handshake can take as long as its timeout, and
	// Receive and ResponseTrailer would wait behind it.
	dialErr := c.call.ensureDialed()
	c.unmarshaler.metadataMu.Lock()
	defer c.unmarshaler.metadataMu.Unlock()
	// Leading-Metadata replaces same-key handshake values rather than appending
	// to them, so this runs after the 101's headers and assigns rather than
	// merging — which also keeps a second call from duplicating them.
	c.responseHeaderOnce.Do(func() {
		if dialErr != nil || c.call.response == nil {
			return
		}
		for key, values := range c.call.response.Header {
			if isWebSocketProtocolHeader(key) {
				continue
			}
			c.responseHeader[key] = values
		}
	})
	maps.Copy(c.responseHeader, c.unmarshaler.header)
	return c.responseHeader.Clone()
}

// ResponseTrailer returns a copy of the response trailers, safe to read while
// Receive runs.
func (c *websocketClientConn) ResponseTrailer() http.Header {
	c.unmarshaler.metadataMu.Lock()
	defer c.unmarshaler.metadataMu.Unlock()
	return c.responseTrailer.Clone()
}

// noteLocalFault records an error this end produced while encoding, as opposed
// to one the wire reported. A failed write is not one: it says the connection
// or the peer went, which §13 has its own codes for.
func (c *websocketClientConn) noteLocalFault(err *connect.Error) {
	if err == nil {
		return
	}
	// Internal is a codec that is missing or refused the message; the send
	// path's ResourceExhausted is sendMaxBytes, which only this end imposes and
	// only on its own writes. A failed write reports neither.
	if err.Code() == connect.CodeInternal || err.Code() == connect.CodeResourceExhausted {
		c.localFault.CompareAndSwap(nil, err)
	}
}

func (c *websocketClientConn) CloseResponse() error {
	c.closeResponseOnce.Do(func() { c.closeResponseErr = c.closeResponse() })
	return c.closeResponseErr
}

func (c *websocketClientConn) closeResponse() error {
	if c.call.wsConn == nil {
		return nil
	}
	if c.call.stopCloseOnDone != nil {
		c.call.stopCloseOnDone()
	}
	c.call.keeper.stop()
	// Deferred so it runs once the close below has ended the read in flight.
	defer c.call.reader.close()
	closeErr := c.closeConn()
	// The background reader answers the server's close frame and closes the
	// connection itself, so an already-closed connection is a completed
	// handshake rather than a failure.
	if errors.Is(closeErr, net.ErrClosed) {
		return nil
	}
	// §13: S completed the RPC, so a server that then drops the connection
	// without a closing handshake has not failed it.
	if c.unmarshaler.endStreamReceived.Load() {
		return nil
	}
	if closeErr == nil {
		return nil
	}
	// §13.1: the connection ended without S, so the RPC failed. It is
	// Unavailable unless this end canceled it or its deadline passed.
	if ctxErr := c.call.ctx.Err(); ctxErr != nil {
		return wsContextError(ctxErr)
	}
	return errorf(connect.CodeUnavailable, "close WebSocket without EndStreamMessage: %v", closeErr)
}

func (c *websocketClientConn) closeConn() error {
	drain := c.call.drain
	// A peer fault wins over a local one: it is what the server can act on.
	if fault := c.unmarshaler.serverFault.Load(); fault != nil {
		return closeWithDrain(c.call.wsConn, drain, fault.code, closeReason(fault.err.Message()))
	}
	if localErr := c.localFault.Load(); localErr != nil {
		// §13.1: the RPC ended on an unexpected condition at this end, not on
		// anything the server did. Sent unconditionally — the peer behaved, so
		// the closing handshake has nothing queued to drain, and the ordinary
		// 1000 below costs the same handshake anyway.
		return closeClientWithError(c.call.wsConn, drain, localErr.Message())
	}
	// Best-effort clean close. Sending 1000 Normal Closure is correct even if
	// Receive has not yet observed the EndStream: the server's completion path
	// has already sent its own close, and Close is a no-op once the handshake
	// has happened.
	return closeWithDrain(c.call.wsConn, drain, websocket.StatusNormalClosure, "")
}

// websocketClientUnmarshaler reads one message per WebSocket frame. It mirrors
// websocketUnmarshaler (server-side) but recognizes the server's S message
// instead of the client's C and M.
type websocketClientUnmarshaler struct {
	frameDecoder

	call *wsClientCall
	// spec and onProtocolError identify and report a server that frames its
	// responses wrongly; see WithClientProtocolErrorHandler.
	spec            connect.Spec
	onProtocolError ClientProtocolErrorHandler
	// stats receives each decoded message's size. Nil when the caller
	// supplied no CallInfo.
	stats *connect.MessageStats

	// endStreamSeen is atomic because Send and CloseSend, which may run
	// concurrently with Receive, read it.
	endStreamSeen atomic.Bool
	// endStreamReceived is set only by a valid S, unlike endStreamSeen, which a
	// failed read also sets. CloseResponse may run concurrently with Receive.
	endStreamReceived atomic.Bool
	// serverFault is set by Receive and read by Close, which the contract
	// allows to run concurrently.
	serverFault    atomic.Pointer[serverFault]
	endStreamError *connect.Error
	trailer        http.Header
	// header is written by Receive and read by Close, which the contract
	// allows to run concurrently, so metadataMu guards it.
	metadataMu sync.Mutex
	header     http.Header
	// sawData gates the ordering rule on the response side: server metadata is
	// "leading" only while no body has arrived.
	sawData bool
	// sawLeadingMetadata records the server's opening M, which must arrive
	// exactly once and before anything else.
	sawLeadingMetadata bool
}

// serverFault is the first failure the server's framing caused: its message is
// the close reason, and code the close code.
type serverFault struct {
	err  *connect.Error
	code websocket.StatusCode
}

// Unmarshal records whether a failure was the peer's doing, which decides how
// the connection is torn down.
func (u *websocketClientUnmarshaler) Unmarshal(message any) *connect.Error {
	for {
		u.fault = FaultUnknown
		wasMetadata, err := u.unmarshal(message)
		// A classified fault is a protocol error whatever its code: §5 has the
		// client close with 3102 for it, not end the RPC with 1000.
		if err != nil && (u.fault != FaultUnknown || peerFault(err)) {
			u.serverFault.CompareAndSwap(nil, &serverFault{err: err, code: clientCloseCode(u.fault)})
		}
		// Gated on the classification rather than peerFault; see the server
		// side. Observation only — err is returned unchanged.
		if u.fault != FaultUnknown && u.onProtocolError != nil {
			u.onProtocolError(u.spec, u.fault, err)
		}
		// An M carries no message for the caller, so read again rather than
		// return one that was never decoded.
		if err == nil && wasMetadata {
			continue
		}
		return err
	}
}

// readFailure turns a failed read into the RPC's error. Every failure ends the
// response stream.
func (u *websocketClientUnmarshaler) readFailure(readerErr error) *connect.Error {
	u.endStreamSeen.Store(true)
	if u.call.keeper.timedOut() {
		u.endStreamError = errorf(
			connect.CodeUnavailable,
			"server sent nothing for %v, not even a Pong (keep-alive timeout)", u.call.keeper.timeout(),
		)
		return u.endStreamError
	}
	// A read that failed while the context was done is cancellation, not a
	// transport fault. Report ctx.Err so callers see Canceled or
	// DeadlineExceeded rather than whatever the socket happened to say.
	if ctxErr := u.call.ctx.Err(); ctxErr != nil {
		u.endStreamError = wsContextError(ctxErr)
		return u.endStreamError
	}
	// The socket's read deadline and the context's timer are separate
	// clocks, so the read can fail a moment before ctx.Err is set. A read
	// that failed at or past the deadline is the deadline whichever fired
	// first; calling it Unavailable would invite a retry that has no time
	// left to run.
	if deadline, ok := u.call.ctx.Deadline(); ok && !time.Now().Before(deadline) {
		u.endStreamError = errorf(connect.CodeDeadlineExceeded, "%w", context.DeadlineExceeded)
		return u.endStreamError
	}
	if isCleanWebSocketClose(readerErr) {
		// A clean close with no EndStreamMessage means the RPC never reached
		// a verdict. Unavailable, not EOF: the caller has no result, and
		// retrying is reasonable.
		u.endStreamError = errorf(
			connect.CodeUnavailable,
			"server closed WebSocket without EndStreamMessage",
		)
		return u.endStreamError
	}
	// Classified like any other peer fault: a server sending more than this
	// client agreed to read is the server's mistake, and the monitor should
	// see it under the same name the server uses.
	if errors.Is(readerErr, errMessageTooBig) {
		u.fault = FaultSizeLimit
		return exceedsReadLimit(u.readMaxBytes)
	}
	if limitErr := peerRejectedAsTooBig(readerErr); limitErr != nil {
		u.fault = FaultSizeLimit
		return limitErr
	}
	// §13: one of the two close codes a client may take an outcome from. The
	// server failed the connection over a text message this client sent.
	if websocket.CloseStatus(readerErr) == websocket.StatusInvalidFramePayloadData {
		return errorf(
			connect.CodeInternal,
			"server rejected a text message from this client as invalid UTF-8: %w", readerErr,
		)
	}
	// %v, not %w: the cause is often io.EOF, which callers read as a normal
	// end of stream rather than a failure.
	return errorf(connect.CodeUnavailable, "read websocket message: %v", readerErr)
}

// unmarshal reads and dispatches one message, and reports whether it was an M.
func (u *websocketClientUnmarshaler) unmarshal(message any) (wasMetadata bool, err *connect.Error) {
	if err := u.call.ensureDialed(); err != nil {
		u.endStreamSeen.Store(true)
		u.endStreamError = err
		return false, err
	}
	if u.endStreamSeen.Load() {
		return false, errorf(connect.CodeUnknown, "%w", io.EOF)
	}
	wire, readErr := u.nextMessage()
	if readErr != nil {
		return false, readErr
	}
	return wire.marker == markerMetadata, u.dispatch(wire, message)
}

// nextMessage reads one frame and returns the message it carries.
func (u *websocketClientUnmarshaler) nextMessage() (wireMessage, *connect.Error) {
	messageType, frame, readerErr := u.call.reader.next(u.call.ctx)
	if readerErr != nil {
		return wireMessage{}, u.readFailure(readerErr)
	}
	wire, parseErr := u.parseFrame(messageType, frame)
	if u.fault == FaultTextEncoding {
		u.endStreamSeen.Store(true)
	}
	return wire, parseErr
}

// dispatch interprets one message and decodes its payload into message.
func (u *websocketClientUnmarshaler) dispatch(wire wireMessage, message any) *connect.Error {
	// Every response stream opens with exactly one M, so anything else first
	// means the peer is not speaking this protocol and there is no partial
	// state worth keeping.
	if !u.sawLeadingMetadata && wire.marker != markerMetadata {
		return u.fail(
			FaultMetadata,
			"server's first message must be M; got %s", markerName(wire.marker),
		)
	}

	switch wire.marker {
	case markerClientEndStream:
		return u.fail(FaultMarker, "server sent %s, which only a client may send", markerName(wire.marker))
	case markerMetadata:
		// §6.1: M and S are always JSON, and a binary frame says the payload
		// is not.
		if !wire.text {
			return u.fail(FaultFrameType, "server sent M in a binary frame; expected a text frame")
		}
		if mergeErr := u.mergeLeadingMetadata(wire.payload); mergeErr != nil {
			return mergeErr
		}
		return nil
	case markerServerEndStream:
		if !wire.text {
			return u.fail(FaultFrameType, "server sent S in a binary frame; expected a text frame")
		}
		return u.endStream(wire.payload)
	case markerBody:
		u.sawData = true
		if err := u.decodeBody(u.call.ctx, u.call.codec, wire, message); err != nil {
			return err
		}
		if u.stats != nil {
			*u.stats = connect.MessageStats{Size: len(wire.payload)}
		}
		return nil
	default:
		return u.fail(FaultMarker, "server sent unknown marker %s", markerName(wire.marker))
	}
}

// endStream records the verdict an S message carries, and returns io.EOF as
// the end of the response stream.
func (u *websocketClientUnmarshaler) endStream(payload []byte) *connect.Error {
	// Parsed unconditionally, so a bare S fails here exactly as a zero-length
	// end-of-stream payload fails in Connect's HTTP streaming protocol.
	// §9: the payload is an EndStreamResponse, a JSON object. json accepts
	// null for a struct, which would read as success.
	if trimmed := bytes.TrimSpace(payload); len(trimmed) > 0 && trimmed[0] != '{' {
		return u.fail(FaultMetadata, "S message is not a JSON EndStreamMessage object; the smallest is {}")
	}
	var end connectwire.EndStreamMessage
	if err := json.Unmarshal(payload, &end); err != nil {
		return u.fail(FaultMetadata, "unmarshal EndStreamMessage: %w", err)
	}
	trailer, decodeErr := decodeTrailer(end.Trailer)
	if decodeErr != nil {
		u.fault = FaultMetadata
		return decodeErr
	}
	u.endStreamSeen.Store(true)
	u.endStreamReceived.Store(true)
	u.trailer = trailer
	u.endStreamError = end.Error.AsError()
	return errorf(connect.CodeUnknown, "%w", io.EOF)
}

// mergeLeadingMetadata folds the server's M message into the response headers.
//
// Exactly one arrives, and it opens the response stream. A second would mean
// the server is rewriting headers the application may already have read, which
// is the same race the request side refuses.
func (u *websocketClientUnmarshaler) mergeLeadingMetadata(payload []byte) *connect.Error {
	if u.sawData {
		return u.fail(
			FaultMetadata,
			"server sent M after a body; metadata is leading only before the first one",
		)
	}
	if u.sawLeadingMetadata {
		return u.fail(
			FaultMetadata,
			"server sent a second M message; a stream carries exactly one, and it opens the stream",
		)
	}
	u.sawLeadingMetadata = true
	entries, parseErr := u.metadataEntries(payload)
	if parseErr != nil {
		return parseErr
	}
	meta, decodeErr := u.metadataHeader(entries)
	if decodeErr != nil {
		return decodeErr
	}
	u.metadataMu.Lock()
	defer u.metadataMu.Unlock()
	if u.header == nil {
		u.header = make(http.Header, len(meta))
	}
	maps.Copy(u.header, meta)
	return nil
}

// wsContextError maps a done context's error to the matching Connect code,
// preserving the original error via %w.
func wsContextError(ctxErr error) *connect.Error {
	if errors.Is(ctxErr, context.Canceled) {
		return errorf(connect.CodeCanceled, "%w", ctxErr)
	}
	return errorf(connect.CodeDeadlineExceeded, "%w", ctxErr)
}

// dialError turns a failed handshake into the RPC's error. §4.5: a Connect
// error in the response body wins, then Connect-over-HTTP's status mapping. A
// 101 that still failed broke the handshake's own rules, such as echoing a
// token this client did not offer, which is the server's fault: Internal.
func dialError(err error, response *http.Response, body []byte) *connect.Error {
	switch {
	case errors.Is(err, context.Canceled):
		return errorf(connect.CodeCanceled, "WebSocket upgrade canceled: %w", err)
	case errors.Is(err, context.DeadlineExceeded):
		return errorf(connect.CodeDeadlineExceeded, "WebSocket upgrade deadline exceeded: %w", err)
	}
	// Callers read io.EOF as a clean end of stream, so a connection dropped
	// mid-handshake is reported as unexpected, as connecthttp does.
	if errors.Is(err, io.EOF) {
		err = fmt.Errorf("%s: %w", err.Error(), io.ErrUnexpectedEOF)
	}
	if response == nil {
		return errorf(connect.CodeUnavailable, "WebSocket upgrade failed: %w", err)
	}
	if response.StatusCode == http.StatusSwitchingProtocols {
		return errorf(connect.CodeInternal, "WebSocket upgrade failed: %w", err)
	}
	if wireErr := handshakeWireError(response, body); wireErr != nil {
		return wireErr
	}
	return errorf(
		httpToCode(response.StatusCode),
		"WebSocket upgrade failed: HTTP %s: %w",
		response.Status, err,
	)
}

// handshakeWireError reads a Connect error from a rejected handshake's body,
// or returns nil if the body is not one. A body without a recognizable code
// is not one either, so the status mapping still applies to it.
func handshakeWireError(response *http.Response, body []byte) *connect.Error {
	mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if !strings.EqualFold(mediaType, "application/json") || len(body) == 0 {
		return nil
	}
	var wireErr connectwire.WireError
	if json.Unmarshal(body, &wireErr) == nil && wireErr.Code != 0 {
		return wireErr.AsError()
	}
	return nil
}

// httpToCode maps an HTTP status to a Connect code. Copied from connecthttp,
// where it is unexported.
//
// https://github.com/grpc/grpc/blob/master/doc/http-grpc-status-mapping.md
// Note that this is NOT the inverse of the gRPC-to-HTTP or Connect-to-HTTP
// mappings.
func httpToCode(httpCode int) connect.Code {
	// Literals are easier to compare to the specification (vs named
	// constants).
	switch httpCode {
	case 400:
		return connect.CodeInternal
	case 401:
		return connect.CodeUnauthenticated
	case 403:
		return connect.CodePermissionDenied
	case 404:
		return connect.CodeUnimplemented
	case 429:
		return connect.CodeUnavailable
	case 502, 503, 504:
		return connect.CodeUnavailable
	default:
		return connect.CodeUnknown
	}
}

// checkNoContextTakeover refuses a handshake that negotiated
// permessage-deflate with a compression context shared across messages.
//
// A shared context leaks plaintext between messages whenever attacker-
// influenced and secret data travel on one connection — the CRIME/BREACH
// family. The server is required to impose no-context-takeover, but a client
// that trusts it to have done so is protected only as far as the peer is
// conforming, and the client's own plaintext is what leaks.
//
// No extension at all is fine: nothing is compressed, so nothing leaks.
func checkNoContextTakeover(response *http.Response) *connect.Error {
	if response == nil {
		return nil
	}
	for _, value := range response.Header.Values(wsHeaderExtensions) {
		for extension := range strings.SplitSeq(value, ",") {
			parameters := strings.Split(extension, ";")
			if strings.TrimSpace(parameters[0]) != "permessage-deflate" {
				continue
			}
			var client, server bool
			for _, parameter := range parameters[1:] {
				switch strings.TrimSpace(parameter) {
				case "client_no_context_takeover":
					client = true
				case "server_no_context_takeover":
					server = true
				}
			}
			if !client || !server {
				return errorf(
					connect.CodeInternal,
					"server negotiated %q without no-context-takeover in both directions; "+
						"a shared compression context leaks plaintext across messages",
					strings.TrimSpace(extension),
				)
			}
		}
	}
	return nil
}

func isWebSocketProtocolHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case wsHeaderUpgrade,
		wsHeaderConnection,
		"Sec-Websocket-Accept",
		wsHeaderProtocol,
		wsHeaderExtensions,
		"Sec-Websocket-Version",
		"Sec-Websocket-Key":
		return true
	}
	return false
}
