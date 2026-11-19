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
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/connectwire"
	"github.com/coder/websocket"
)

// websocketClientConn is the client's view of one RPC. [clientStream] adapts
// it to [connect.ClientStream]. It mirrors the server: one message per frame,
// with [messageWriter] marshaling outbound and the conn reading inbound.
type websocketClientConn struct {
	frameDecoder

	// ctx is the RPC's context. It lives here because Send, Receive, and the
	// other stream methods take none.
	ctx context.Context //nolint:containedctx

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
	callInfo *connect.CallInfo

	// The read side takes one message per WebSocket frame and recognizes the
	// server's M and S messages.

	// spec and onProtocolError identify and report a server that frames its
	// responses wrongly; see WithClientProtocolErrorHandler.
	spec            connect.Spec
	onProtocolError ClientProtocolErrorHandler
	// receiveStats receives each decoded message's size. Nil when the caller
	// supplied no CallInfo.
	receiveStats *connect.MessageStats

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

	// responseHeader and responseTrailer are guarded by metadataMu, since
	// Close may read them while Receive writes them.
	responseHeader  http.Header
	responseTrailer http.Header

	responseHeaderOnce sync.Once
	// Close may run concurrently with Receive by contract, and both publish.
	// A sync.Once rather than a flag: the ordering that makes a plain bool
	// safe today is incidental — Close publishes before touching the conn and
	// Receive after — and moving either call would break it silently.
	publishOnce        sync.Once
	publishHeadersOnce sync.Once
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
		if c.endStreamSeen.Load() {
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
		c.openErr = c.marshaler.writeJSON(c.ctx, markerMetadata, entries)
		// Writing M is what dials, so the server's codec choice is known now.
		// A failed dial leaves nothing to report.
		if !c.call.connected() {
			return
		}
		if c.callInfo != nil {
			c.callInfo.Codec = c.call.negotiatedCodec().Name()
		}
	})
	return c.openErr
}

// SendHeaders dials and writes the M message that opens the stream, which is
// what actually transmits the request metadata.
func (c *websocketClientConn) SendHeaders() error {
	if err := c.call.ensureDialed(c.ctx); err != nil {
		return err
	}
	if err := c.open(); err != nil {
		return err
	}
	return nil // literal nil; a nil *connect.Error is a non-nil error
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
	if c.endStreamSeen.Load() {
		return errorf(connect.CodeUnknown, "send after the server ended the stream: %w", io.EOF)
	}
	if err := c.open(); err != nil {
		return err
	}
	if err := c.marshaler.writeBody(c.ctx, c.call.negotiatedCodec(), msg); err != nil {
		c.noteLocalFault(err)
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

func (c *websocketClientConn) CloseRequest() error {
	// §8: no C once the response stream has ended. Writing one would race
	// this end's reply to the server's Close frame.
	if c.endStreamSeen.Load() {
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
		c.sendCloseErr = c.marshaler.send(c.ctx, markerClientEndStream, true, nil)
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
	return errors.Is(err, io.EOF) || c.ctx.Err() != nil
}

func (c *websocketClientConn) Receive(msg any) error {
	// A stream that only receives still has to open: the server will not
	// dispatch the handler until the opening metadata arrives. If the server
	// has already ended the stream, its verdict is still there to read.
	if err := c.open(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	err := c.receive(msg)
	if err == nil {
		return nil
	}
	// The trailers go on the conn, which the transport publishes to CallInfo,
	// rather than on the error.
	if c.endStreamSeen.Load() {
		c.metadataMu.Lock()
		mergeHeaders(c.responseTrailer, c.trailer)
		c.metadataMu.Unlock()
		if c.endStreamReceived.Load() {
			c.releaseAfterEndOfStream()
		}
		if serverErr := c.endStreamError; serverErr != nil {
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

// preventDial makes sure this stream never dials, and reports whether it
// already had.
func (c *websocketClientConn) preventDial() bool {
	return c.call.preventDial()
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
	closeErr := c.call.closeWith(c.closeStatus())
	// The background reader answers the server's close frame and closes the
	// connection itself, so an already-closed connection is a completed
	// handshake rather than a failure.
	if errors.Is(closeErr, net.ErrClosed) {
		return nil
	}
	// §13: S completed the RPC, so a server that then drops the connection
	// without a closing handshake has not failed it.
	if c.endStreamReceived.Load() {
		return nil
	}
	if closeErr == nil {
		return nil
	}
	// §13.1: the connection ended without S, so the RPC failed. It is
	// Unavailable unless this end canceled it or its deadline passed.
	if ctxErr := c.ctx.Err(); ctxErr != nil {
		return wsContextError(ctxErr)
	}
	return errorf(connect.CodeUnavailable, "close WebSocket without EndStreamMessage: %v", closeErr)
}

// closeStatus picks the code and reason this end closes the connection with.
func (c *websocketClientConn) closeStatus() (websocket.StatusCode, string) {
	// A peer fault wins over a local one: it is what the server can act on.
	if fault := c.serverFault.Load(); fault != nil {
		return fault.code, closeReason(fault.err.Message())
	}
	if localErr := c.localFault.Load(); localErr != nil {
		// §13.1: the RPC ended on an unexpected condition at this end, not on
		// anything the server did. Sent unconditionally — the peer behaved, so
		// the closing handshake has nothing queued to drain, and the ordinary
		// 1000 below costs the same handshake anyway. §13 requires the reason.
		return wsStatusClientProtocolError, closeReason(localErr.Message())
	}
	// Best-effort clean close. Sending 1000 Normal Closure is correct even if
	// Receive has not yet observed the EndStream: the server's completion path
	// has already sent its own close, and Close is a no-op once the handshake
	// has happened.
	return websocket.StatusNormalClosure, ""
}

// Unmarshal records whether a failure was the peer's doing, which decides how
// the connection is torn down.
func (c *websocketClientConn) receive(message any) *connect.Error {
	for {
		c.fault = faultNone
		wasMetadata, err := c.unmarshal(message)
		// A classified fault is a protocol error whatever its code: §5 has the
		// client close with 3102 for it, not end the RPC with 1000.
		if err != nil && (c.fault != faultNone || peerFault(err)) {
			c.serverFault.CompareAndSwap(nil, &serverFault{err: err, code: clientCloseCode(c.fault)})
		}
		// Gated on the classification rather than peerFault; see the server
		// side. Observation only — err is returned unchanged.
		if c.fault != faultNone && c.onProtocolError != nil {
			c.onProtocolError(c.spec, c.fault, err)
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
func (c *websocketClientConn) readFailure(readerErr error) *connect.Error {
	c.endStreamSeen.Store(true)
	if timeout, expired := c.call.keepAliveExpired(); expired {
		c.endStreamError = errorf(
			connect.CodeUnavailable,
			"server sent nothing for %v, not even a Pong (keep-alive timeout)", timeout,
		)
		return c.endStreamError
	}
	// A read that failed while the context was done is cancellation, not a
	// transport fault. Report ctx.Err so callers see Canceled or
	// DeadlineExceeded rather than whatever the socket happened to say.
	if ctxErr := c.ctx.Err(); ctxErr != nil {
		c.endStreamError = wsContextError(ctxErr)
		return c.endStreamError
	}
	// The socket's read deadline and the context's timer are separate
	// clocks, so the read can fail a moment before ctx.Err is set. A read
	// that failed at or past the deadline is the deadline whichever fired
	// first; calling it Unavailable would invite a retry that has no time
	// left to run.
	if deadline, ok := c.ctx.Deadline(); ok && !time.Now().Before(deadline) {
		c.endStreamError = errorf(connect.CodeDeadlineExceeded, "%w", context.DeadlineExceeded)
		return c.endStreamError
	}
	if isCleanWebSocketClose(readerErr) {
		// A clean close with no EndStreamMessage means the RPC never reached
		// a verdict. Unavailable, not EOF: the caller has no result, and
		// retrying is reasonable.
		c.endStreamError = errorf(
			connect.CodeUnavailable,
			"server closed WebSocket without EndStreamMessage",
		)
		return c.endStreamError
	}
	// Classified like any other peer fault: a server sending more than this
	// client agreed to read is the server's mistake, and the monitor should
	// see it under the same name the server uses.
	if errors.Is(readerErr, errMessageTooBig) {
		c.fault = FaultSizeLimit
		return exceedsReadLimit(c.readMaxBytes)
	}
	if limitErr := peerRejectedAsTooBig(readerErr); limitErr != nil {
		c.fault = FaultSizeLimit
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
func (c *websocketClientConn) unmarshal(message any) (wasMetadata bool, err *connect.Error) {
	if err := c.call.ensureDialed(c.ctx); err != nil {
		c.endStreamSeen.Store(true)
		c.endStreamError = err
		return false, err
	}
	if c.endStreamSeen.Load() {
		return false, errorf(connect.CodeUnknown, "%w", io.EOF)
	}
	wire, readErr := c.nextMessage()
	if readErr != nil {
		return false, readErr
	}
	return wire.marker == markerMetadata, c.dispatch(wire, message)
}

// nextMessage reads one frame and returns the message it carries.
func (c *websocketClientConn) nextMessage() (wireMessage, *connect.Error) {
	messageType, frame, readerErr := c.call.nextFrame(c.ctx)
	if readerErr != nil {
		return wireMessage{}, c.readFailure(readerErr)
	}
	wire, parseErr := c.parseFrame(messageType, frame)
	if c.fault == FaultTextEncoding {
		c.endStreamSeen.Store(true)
	}
	return wire, parseErr
}

// dispatch interprets one message and decodes its payload into message.
func (c *websocketClientConn) dispatch(wire wireMessage, message any) *connect.Error {
	// Every response stream opens with exactly one M, so anything else first
	// means the peer is not speaking this protocol and there is no partial
	// state worth keeping.
	if !c.sawLeadingMetadata && wire.marker != markerMetadata {
		return c.fail(
			FaultMetadata,
			"server's first message must be M; got %s", markerName(wire.marker),
		)
	}

	switch wire.marker {
	case markerClientEndStream:
		return c.fail(FaultMarker, "server sent %s, which only a client may send", markerName(wire.marker))
	case markerMetadata:
		// §6.1: M and S are always JSON, and a binary frame says the payload
		// is not.
		if !wire.text {
			return c.fail(FaultFrameType, "server sent M in a binary frame; expected a text frame")
		}
		if mergeErr := c.mergeLeadingMetadata(wire.payload); mergeErr != nil {
			return mergeErr
		}
		return nil
	case markerServerEndStream:
		if !wire.text {
			return c.fail(FaultFrameType, "server sent S in a binary frame; expected a text frame")
		}
		return c.endStream(wire.payload)
	case markerBody:
		c.sawData = true
		if err := c.decodeBody(c.ctx, c.call.negotiatedCodec(), wire, message); err != nil {
			return err
		}
		if c.receiveStats != nil {
			*c.receiveStats = connect.MessageStats{Size: len(wire.payload)}
		}
		return nil
	default:
		return c.fail(FaultMarker, "server sent unknown marker %s", markerName(wire.marker))
	}
}

// endStream records the verdict an S message carries, and returns io.EOF as
// the end of the response stream.
func (c *websocketClientConn) endStream(payload []byte) *connect.Error {
	// Parsed unconditionally, so a bare S fails here exactly as a zero-length
	// end-of-stream payload fails in Connect's HTTP streaming protocol.
	// §9: the payload is an EndStreamResponse, a JSON object. json accepts
	// null for a struct, which would read as success.
	if trimmed := bytes.TrimSpace(payload); len(trimmed) > 0 && trimmed[0] != '{' {
		return c.fail(FaultMetadata, "S message is not a JSON EndStreamMessage object; the smallest is {}")
	}
	var end connectwire.EndStreamMessage
	if err := json.Unmarshal(payload, &end); err != nil {
		return c.fail(FaultMetadata, "unmarshal EndStreamMessage: %w", err)
	}
	trailer, decodeErr := decodeTrailer(end.Trailer)
	if decodeErr != nil {
		c.fault = FaultMetadata
		return decodeErr
	}
	c.endStreamSeen.Store(true)
	c.endStreamReceived.Store(true)
	c.trailer = trailer
	c.endStreamError = end.Error.AsError()
	return errorf(connect.CodeUnknown, "%w", io.EOF)
}

// serverFault is the first failure the server's framing caused: its message is
// the close reason, and code the close code.
type serverFault struct {
	err  *connect.Error
	code websocket.StatusCode
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
	case faultNone, FaultMarker, FaultFrameType, FaultMetadata, FaultMessageEncoding:
		return wsStatusClientProtocolError
	}
	return wsStatusClientProtocolError
}

func isCleanWebSocketClose(err error) bool {
	// Not a switch: the repo's exhaustive linter wants every StatusCode listed,
	// and enumerating thirteen irrelevant close codes would obscure the rule.
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}
