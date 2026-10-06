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
	"connectrpc.com/connect/v2/internal/connectwire"
	"github.com/coder/websocket"
)

// Marker framing over WebSocket frames, for both directions.
//
// Each WebSocket frame carries exactly one message: a single byte below 0x80
// naming its kind, then the payload. There is no length prefix — the frame is
// the boundary — and the frame type says how the payload is encoded, binary
// for Protobuf and text for JSON.
//
// Two markers are client-only, because WebSocket lacks two things HTTP
// provides:
//
//   - C (End-Of-Client-Stream) stands in for request-body EOF, which a
//     WebSocket cannot half-close.
//   - M (Leading-Metadata) carries a JSON-encoded http.Header, for browsers,
//     whose WebSocket API cannot set headers on the upgrade. Every stream opens
//     with exactly one, in either direction.
//
// The server answers with zero or more B messages and then one S message,
// whose payload is the EndStreamMessage JSON that Connect's HTTP streaming
// protocol also uses.

const (
	// ProtocolConnectWebSocket identifies the WebSocket transport for the
	// Connect protocol on the [connect.CallInfo].Protocol field.
	ProtocolConnectWebSocket = "connect+ws"

	wsSubprotocolProto = "connectrpc.1+proto"
	wsSubprotocolJSON  = "connectrpc.1+json"

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
	request *http.Request
	wsConn  *websocket.Conn
	// faultCloseCode sends the close frame §13 owes a peer that broke the
	// framing, instead of hanging up on it.
	faultCloseCode bool

	marshaler   messageWriter
	unmarshaler websocketUnmarshaler

	// callInfo is read at flush time rather than copied in: a handler sets its
	// leading metadata during the call, and the conn sends it before the first
	// message, so there is no point at which a snapshot would be current.
	callInfo        *connect.CallInfo
	responseTrailer http.Header
	leadingSent     bool
	// closeCtx writes the terminal messages. Bodies go out under the RPC's
	// context; the S message cannot, because the commonest reason to send one
	// is that the context just expired.
	closeCtx context.Context //nolint:containedctx
}

func (c *websocketHandlerConn) Receive(msg any) error {
	if err := c.unmarshaler.Unmarshal(msg); err != nil {
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

func (c *websocketHandlerConn) Send(msg any) error {
	if err := c.flushLeadingMetadata(); err != nil {
		return err
	}
	if err := c.marshaler.writeBody(msg); err != nil {
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

// terminalMarshaler copies the marshaler onto the detached close context, so
// the final messages are not written under a context whose expiry is the very
// thing being reported.
func (c *websocketHandlerConn) terminalMarshaler() messageWriter {
	terminal := c.marshaler
	terminal.ctx = c.closeCtx
	terminal.sender = &websocketBinarySender{ctx: c.closeCtx, conn: c.wsConn}
	return terminal
}

// flushLeadingMetadata writes the server's M message. It must run before the
// first body and before the S message alike: a stream that fails without
// sending a body is where leading metadata is most wanted, and there would be
// nothing else to carry it.
//
// It goes out even when there is no metadata, as {}. Every stream opens with
// exactly one M in each direction, so a client can read its first message
// without a branch for the empty case — and can take that message as the
// signal that the server has accepted the stream, which is what the response
// header block gives it for free over HTTP.
func (c *websocketHandlerConn) flushLeadingMetadata() *connect.Error {
	if c.leadingSent {
		return nil
	}
	c.leadingSent = true
	header := make(http.Header)
	if c.callInfo != nil {
		for key, values := range c.callInfo.ResponseHeader().All() {
			header[http.CanonicalHeaderKey(key)] = values
		}
	}
	return c.marshaler.writeJSON(markerMetadata, encodeMetadata(header))
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
	if c.unmarshaler.leftAfterEndOfStream.Load() {
		return true
	}
	if c.unmarshaler.eof && !c.unmarshaler.sawEndOfClientStream {
		return true
	}
	// The reader runs ahead of the handler and of the post-C drain, so it can
	// see the connection end before either has read the failure.
	return c.unmarshaler.reader.connectionEnded(peerGoneGrace)
}

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

// Close codes this binding assigns a client, from §13. A browser script may
// only pass 1000 or 3000-4999 to close(), so a client cannot send the 10xx code
// a server would; its codes live in the 31xx range instead, and a receiver
// folds them onto the 10xx code with the same last two digits.
const (
	wsStatusClientProtocolError   = websocket.StatusCode(3102)
	wsStatusClientUnsupportedData = websocket.StatusCode(3103)
	wsStatusClientMessageTooBig   = websocket.StatusCode(3109)
	wsStatusClientCompression     = websocket.StatusCode(3110)
	wsStatusClientInternal        = websocket.StatusCode(3111)
)

// normalizeCloseStatus folds a 31xx code onto the 10xx code with the same last
// two digits, so that the rest of this package can interpret one set (§13).
func normalizeCloseStatus(status websocket.StatusCode) websocket.StatusCode {
	if status >= 3100 && status <= 3199 {
		return status - 2100
	}
	return status
}

// serverCloseCode is the code §13 assigns each framing fault. A more specific
// one wins over the generic protocol error.
func serverCloseCode(fault ProtocolFault) websocket.StatusCode {
	switch fault {
	case FaultFrameType:
		return websocket.StatusUnsupportedData
	case FaultSizeLimit:
		return websocket.StatusMessageTooBig
	case FaultMarker, FaultMetadata, FaultMessageEncoding, FaultUnknown:
		return websocket.StatusProtocolError
	}
	return websocket.StatusProtocolError
}

// clientCloseCode is the 31xx counterpart of serverCloseCode.
func clientCloseCode(fault ProtocolFault) websocket.StatusCode {
	switch fault {
	case FaultFrameType:
		return wsStatusClientUnsupportedData
	case FaultSizeLimit:
		return wsStatusClientMessageTooBig
	case FaultMarker, FaultMetadata, FaultMessageEncoding, FaultUnknown:
		return wsStatusClientProtocolError
	}
	return wsStatusClientProtocolError
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

func (c *websocketHandlerConn) Close(err error) error {
	// A message after C is the RPC's outcome whatever the handler concluded:
	// the peer broke the framing, and §8 makes that a protocol error.
	if afterC := c.unmarshaler.afterEndOfStream.Load(); afterC != nil {
		err = afterC
		c.unmarshaler.peerFaulted = true
		c.unmarshaler.fault = FaultMarker
	}
	// A deadline that fired is what produced most of the errors reaching here,
	// and writing under the expired context would fail — leaving the peer with
	// a closed connection and no verdict, which it can only report as
	// Unavailable. Detach for the final write.
	if c.closeCtx != nil {
		c.marshaler = c.terminalMarshaler()
	}
	// §8: a connection that ended before S means the caller can no longer
	// receive one, so none is attempted. Gated on the drain's own signal, not
	// on peerGone: eof is set by any failed read, including an oversized
	// message this end refused while its sender waits for the verdict.
	if c.unmarshaler.leftAfterEndOfStream.Load() {
		_ = c.wsConn.CloseNow()
		return nil
	}
	// Leading metadata precedes the S message even when no body was
	// sent, so headers stay headers rather than folding into the trailers.
	marshalErr := c.flushLeadingMetadata()
	if marshalErr == nil {
		marshalErr = c.marshaler.writeEndStream(err, c.responseTrailer)
	}
	marshalErr = c.terminalWriteError(marshalErr)
	closeCode := websocket.StatusNormalClosure
	var closeMessage string
	switch {
	case marshalErr != nil:
		closeCode = websocket.StatusInternalError
		closeMessage = marshalErr.Message()
	case c.unmarshaler.peerFaulted:
		// The S message carries the verdict; the code says which kind of
		// mistake it was to a peer that reads codes and not JSON — which is
		// every browser script.
		closeCode = serverCloseCode(c.unmarshaler.fault)
		if connectErr, ok := errors.AsType[*connect.Error](err); ok {
			closeMessage = connectErr.Message()
		}
	}
	// A peer that behaved gets the closing handshake. One that broke the
	// framing is hung up on, which §13 does not ask for: it assigns that peer
	// 1002, 1003 or 1009 here.
	//
	// The handshake reads until the peer's own close frame arrives, bounded at
	// five seconds but by no number of bytes, and a peer that just overran the
	// read limit has bytes queued to spend them on — measured, a rejected 8MiB
	// bomb was drained in full, where hanging up stopped at 96KiB. The S
	// message above already carries the verdict; the code is what is given up,
	// and WithFaultCloseCode sends it where that matters more.
	if c.unmarshaler.peerFaulted && !c.faultCloseCode {
		_ = c.wsConn.CloseNow()
	} else {
		_ = c.wsConn.Close(closeCode, closeReason(closeMessage))
	}
	if marshalErr != nil {
		return marshalErr
	}
	return nil
}

// websocketBinarySender is the server's [messageSender]: it writes each
// message as one WebSocket frame rather than appending it to an HTTP response
// body.
type websocketBinarySender struct {
	ctx  context.Context //nolint:containedctx
	conn *websocket.Conn
}

func (s *websocketBinarySender) send(text bool, data []byte) (int64, error) {
	return writeFrame(s.ctx, s.conn, text, data)
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
	ctx    context.Context //nolint:containedctx
	reader *frameReader
	// keeper is consulted only to say why the connection ended. Nil when
	// keep-alive is off.
	keeper *keepAlive
	// callInfo receives M messages. They arrive after the
	// handshake, so this is the only way a browser's metadata reaches the
	// handler.
	callInfo     *connect.CallInfo
	codecs       codecPair
	readMaxBytes int
	// bodyIsText is the frame type the subprotocol negotiated. A body in the
	// other one is a protocol error even when this end holds both codecs.
	bodyIsText bool
	// infrastructureHeaders names what this deployment's own proxies set, and
	// so what a client may not send in an M message.
	infrastructureHeaders []string
	// info is carried so a fault can be attributed to the connection that
	// produced it; its OnProtocolError is the monitor.
	info SessionInfo

	// fault classifies the most recent peer fault for OnProtocolError.
	fault ProtocolFault
	// eof stops the read path; sawEndOfClientStream says why it stopped. A
	// client that sent C finished, and one whose connection died did not —
	// the same condition at the same call site, told apart only by this.
	eof                  bool
	sawEndOfClientStream bool
	peerFaulted          bool
	// discardOnce guards the post-C drain, which must not be started twice.
	discardOnce sync.Once
	// leftAfterEndOfStream is set by that drain when the connection ends. The
	// handler has had its io.EOF and is not reading, so nothing else can tell
	// it the caller is gone.
	leftAfterEndOfStream atomic.Bool
	// cancelCall ends the RPC when that happens, which is how §8.1 says the end
	// of the connection reaches a handler.
	cancelCall context.CancelFunc
	// afterEndOfStream carries a post-C protocol error from the drain
	// goroutine to Close, which is the only place it can still be reported.
	afterEndOfStream atomic.Pointer[connect.Error]
}

// Unmarshal records whether a failure was the peer's doing, which decides how
// the connection is torn down.
func (u *websocketUnmarshaler) Unmarshal(message any) *connect.Error {
	u.fault = FaultUnknown
	err := u.unmarshal(message)
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
	if peerFault(err) {
		u.peerFaulted = true
	}
	if u.fault != FaultUnknown && u.info.OnProtocolError != nil {
		u.info.OnProtocolError(u.info, u.fault, err)
	}
}

// fail records what kind of fault this is before building its error, so a
// monitor can tell them apart without matching message strings.
//
// The code is always InvalidArgument: every fault built here is malformed
// input from the peer. Faults with another code — a read limit, say — come
// from elsewhere and set u.fault directly.
func (u *websocketUnmarshaler) fail(
	fault ProtocolFault,
	format string,
	args ...any,
) *connect.Error {
	u.fault = fault
	return errorf(connect.CodeInvalidArgument, format, args...)
}

func (u *websocketUnmarshaler) unmarshal(message any) *connect.Error {
	if u.eof {
		return u.endOfStreamError()
	}
	wire, readErr := u.nextMessage()
	if readErr != nil {
		return readErr
	}
	return u.dispatch(wire, message)
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
func (u *websocketUnmarshaler) nextMessage() (wireMessage, *connect.Error) {
	messageType, frame, readerErr := u.reader.next(u.ctx)
	if readerErr != nil {
		u.eof = true
		if errors.Is(readerErr, errMessageTooBig) {
			u.fault = FaultSizeLimit
			return wireMessage{}, exceedsReadLimit(u.readMaxBytes)
		}
		if limitErr := readLimitError(readerErr, u.readMaxBytes); limitErr != nil {
			u.fault = FaultSizeLimit
			return wireMessage{}, limitErr
		}
		return wireMessage{}, u.endOfStreamError()
	}
	text := messageType == websocket.MessageText
	if !text && messageType != websocket.MessageBinary {
		return wireMessage{}, u.fail(FaultFrameType, "unknown WebSocket message type %d", messageType)
	}
	wire, decodeErr := decodeMessage(frame, text)
	if decodeErr != nil {
		u.fault = FaultMarker
		return wireMessage{}, decodeErr
	}
	if utf8Err := textEncodingError(frame, text); utf8Err != nil {
		u.fault = FaultMessageEncoding
		return wireMessage{}, utf8Err
	}
	if len(wire.payload) > u.readMaxBytes && u.readMaxBytes > 0 {
		u.fault = FaultSizeLimit
		return wireMessage{}, errorf(
			connect.CodeResourceExhausted,
			"message size %d is larger than configured max %d", len(wire.payload), u.readMaxBytes,
		)
	}
	return wire, nil
}

// dispatch interprets one message and decodes its payload into message.
func (u *websocketUnmarshaler) dispatch(wire wireMessage, message any) *connect.Error {
	switch wire.marker {
	case markerMetadata:
		// The opening message was consumed before dispatch; a second one would
		// write CallInfo while the handler is already reading it.
		return u.fail(
			FaultMetadata,
			"client sent a second M message; a stream carries exactly one, and it opens the stream",
		)
	case markerClientEndStream:
		// The last message from the client. If it carries a body, deliver it;
		// mark EOF either way so the next Receive returns io.EOF.
		u.eof = true
		u.sawEndOfClientStream = true
		u.discardAfterEndOfStream()
		if len(wire.payload) == 0 {
			return errorf(connect.CodeUnknown, "%w", io.EOF)
		}
		return u.decodeBody(wire, message)
	case markerBody:
		return u.decodeBody(wire, message)
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
// goroutine ends when the context is done or the connection closes.
func (u *websocketUnmarshaler) discardAfterEndOfStream() {
	u.discardOnce.Do(func() {
		go func() {
			for {
				if _, _, err := u.reader.next(u.ctx); err != nil {
					// §8: the RPC is canceled once the connection ends, whether
					// or not C arrived — the handler is otherwise computing a
					// response for a peer that cannot receive it.
					//
					// Only when this end had not already given up: a read that
					// failed because the deadline passed says nothing about the
					// peer, and the server still owes it that verdict.
					if u.ctx.Err() == nil {
						u.leftAfterEndOfStream.Store(true)
						if u.cancelCall != nil {
							u.cancelCall()
						}
					}
					return
				}
				// §8: after C the only frames left are Close, Ping and Pong, so
				// a message is a protocol error. Reading goes on regardless, so
				// the peer reaches the verdict rather than stalling on a full
				// buffer before it can read one.
				u.afterEndOfStream.CompareAndSwap(nil, errorf(
					connect.CodeInvalidArgument,
					"client sent a message after its C message",
				))
			}
		}()
	})
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
func (u *websocketUnmarshaler) drainLeadingMetadata() *connect.Error {
	u.fault = FaultUnknown
	wire, readErr := u.nextMessage()
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
	mergeErr := u.mergeLeadingMetadata(wire.payload)
	if mergeErr != nil {
		u.reportFault(mergeErr)
	}
	return mergeErr
}

// decodeBody decodes a body payload with the codec its frame type names.
func (u *websocketUnmarshaler) decodeBody(wire wireMessage, message any) *connect.Error {
	if wire.text != u.bodyIsText {
		// The subprotocol named one encoding and the frame names another. This
		// end may well hold a codec for what arrived, which is exactly why it
		// has to be refused rather than decoded: accepting it would let a peer
		// choose an encoding the handshake did not agree on (§6.1).
		return u.fail(
			FaultFrameType,
			"client sent a %s body on a %s connection",
			encodingForFrame(wire.text), encodingForFrame(u.bodyIsText),
		)
	}
	if len(wire.payload) == 0 {
		if wire.text {
			// An empty JSON body is "{}", never zero bytes, so a text frame
			// carrying only a marker says nothing the codec could decode.
			return u.fail(FaultFrameType, "empty body in a text frame; an empty JSON message is {}")
		}
		// The empty Protobuf message: the zero value is correct.
		return nil
	}
	codec := u.codecs.forFrame(wire.text)
	if codec == nil {
		return u.fail(
			FaultFrameType,
			"client sent a %s body, which this server was not configured to decode",
			encodingForFrame(wire.text),
		)
	}
	if err := codec.UnmarshalRead(u.ctx, bytes.NewReader(wire.payload), message); err != nil {
		return u.fail(FaultMessageEncoding, "unmarshal message: %w", err)
	}
	return nil
}

func (u *websocketUnmarshaler) mergeLeadingMetadata(payload []byte) *connect.Error {
	// The empty object, never zero bytes: an M carries a JSON object, and a
	// bare marker is not one.
	if len(payload) == 0 {
		return u.fail(FaultMetadata, "empty M message; an empty metadata object is {}")
	}
	if dupErr := duplicateMetadataKey(payload); dupErr != nil {
		u.fault = FaultMetadata
		return dupErr
	}
	var wire map[string][]string
	if err := json.Unmarshal(payload, &wire); err != nil {
		return u.fail(FaultMetadata, "unmarshal M message: %w", err)
	}
	// Checked against the keys as sent, before canonicalization, so the error
	// names the spelling the client used rather than an internal form of it.
	// A reserved key ends the RPC rather than being dropped: a client that
	// believed it had set an identity header would otherwise never learn that
	// the server does not have it.
	for key := range wire {
		if reason, reserved := reservedHeaderReason(key, u.infrastructureHeaders); reserved {
			return u.fail(
				FaultMetadata,
				"client set %q in its M message, which is %s", key, reason,
			)
		}
	}
	meta, decodeErr := decodeMetadata(wire)
	if decodeErr != nil {
		u.fault = FaultMetadata
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
// marker that precedes it.
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
	// The marker is one byte; the payload limit is the rest.
	return int64(readMaxBytes) + 1
}

// errMessageTooBig reports a message that overran the limit this side
// enforces. The reader knows the wire limit; only the caller knows the
// configured one worth naming, so the sentinel carries no number.
var errMessageTooBig = errors.New("message exceeds read limit")

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
) (websocket.MessageType, []byte, error) {
	messageType, reader, err := conn.Reader(ctx)
	if err != nil {
		return 0, nil, err
	}
	if limit < 0 {
		data, err := io.ReadAll(reader)
		return messageType, data, err
	}
	// One byte past the limit is all it takes to know the message overran, and
	// reading more is what the limit exists to prevent.
	var message bytes.Buffer
	switch _, err := io.CopyN(&message, reader, limit+1); {
	case err == nil:
		return messageType, nil, errMessageTooBig
	case errors.Is(err, io.EOF):
		return messageType, message.Bytes(), nil
	default:
		return messageType, nil, err
	}
}

// readLimitError recognizes a message that blew a read limit, whether this
// side enforced it or the peer closed the connection because of it. Both mean
// the same thing to a caller, and neither is a transport failure.
//
// Only this side's limit is reported as a number. A peer's is its own
// business, and its close reason is the only account of it there is.
func readLimitError(err error, readMaxBytes int) *connect.Error {
	if normalizeCloseStatus(websocket.CloseStatus(err)) == websocket.StatusMessageTooBig {
		return errorf(connect.CodeResourceExhausted, "peer rejected message as too big: %w", err)
	}
	if errors.Is(err, websocket.ErrMessageTooBig) {
		return exceedsReadLimit(readMaxBytes)
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
	status = normalizeCloseStatus(status)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}

// The client mirrors the server: one message per frame, with [messageWriter]
// marshaling outbound and websocketClientUnmarshaler reading inbound and
// recognizing the server's S message.

// subprotocolForCodec maps a codec to the token that names it. The client
// always offers the explicit token, so the server's codec choice cannot drift
// from the one the client is encoding with.
func subprotocolForCodec(name string) string {
	if name == connect.CodecNameJSON {
		return wsSubprotocolJSON
	}
	return wsSubprotocolProto
}

// wsClientCall owns the lazy dial and is the [messageSender] the
// writer sends through. Dialing on first use rather than at construction is
// what lets a stream be created without a round trip.
type wsClientCall struct {
	ctx              context.Context //nolint:containedctx
	dialOptions      *websocket.DialOptions
	url              *url.URL
	subprotocol      string
	handshakeTimeout time.Duration

	readLimit int64
	// faultCloseCode spends the closing handshake to hand the server a close
	// code; see websocketHandlerConn.Close for what that costs.
	faultCloseCode bool
	// keepAliveInterval is how often to ping once dialed; see WithKeepAlive.
	keepAliveInterval time.Duration

	dialOnce sync.Once
	dialDone chan struct{}
	wsConn   *websocket.Conn
	reader   *frameReader
	keeper   *keepAlive
	response *http.Response
	dialErr  *connect.Error
}

func (c *wsClientCall) ensureDialed() *connect.Error {
	c.dialOnce.Do(func() {
		defer close(c.dialDone)
		// The handshake gets its own deadline; the connection outlives it.
		dialCtx := c.ctx
		if c.handshakeTimeout > 0 {
			var cancelDial context.CancelFunc
			dialCtx, cancelDial = context.WithTimeout(c.ctx, c.handshakeTimeout)
			defer cancelDial()
		}
		conn, response, err := websocket.Dial(dialCtx, c.url.String(), c.dialOptions)
		c.response = response
		if response != nil && response.Body != nil {
			// We only need the upgrade response's headers; close the body to
			// avoid leaking it. The body is a buffered remainder, not the
			// upgraded connection (that's returned separately as conn).
			_ = response.Body.Close()
		}
		if err != nil {
			c.dialErr = dialError(err, response)
			return
		}
		// CloseNow, not Close: the closing handshake waits for the peer's own
		// close frame, and a server that just failed the handshake's terms is
		// the last peer worth waiting five seconds on. Same policy as a peer
		// that faults mid-stream.
		if got := conn.Subprotocol(); got != c.subprotocol {
			_ = conn.CloseNow()
			c.dialErr = errorf(
				connect.CodeInternal,
				"server selected unexpected Sec-WebSocket-Protocol %q (want %q)",
				got, c.subprotocol,
			)
			return
		}
		if takeoverErr := checkNoContextTakeover(response); takeoverErr != nil {
			// §10 assigns this 3110, and sending it means completing a closing
			// handshake that a server which just failed the handshake's terms
			// has no reason to answer — five seconds of dial latency for a
			// connection that is already lost. Same trade as a mid-stream
			// fault, so the same option decides it.
			if c.faultCloseCode {
				_ = conn.Close(wsStatusClientCompression, "no-context-takeover required")
			} else {
				_ = conn.CloseNow()
			}
			c.dialErr = takeoverErr
			return
		}
		// Bounds the inflated message and the compressed input both; see the
		// note in session.Serve.
		conn.SetReadLimit(c.readLimit)
		c.wsConn = conn
		c.reader = newFrameReader(c.ctx, conn.Read)
		c.keeper = startKeepAlive(c.ctx, conn, c.reader, c.keepAliveInterval, wsStatusClientInternal)
	})
	<-c.dialDone
	return c.dialErr
}

// send implements [messageSender], writing one message as a single WebSocket
// frame.
func (c *wsClientCall) send(text bool, data []byte) (int64, error) {
	if err := c.ensureDialed(); err != nil {
		return 0, err
	}
	return writeFrame(c.ctx, c.wsConn, text, data)
}

type websocketClientConn struct {
	call   *wsClientCall
	codecs codecPair
	// faultCloseCode mirrors the server's; see websocketHandlerConn.
	faultCloseCode bool
	// localFault records a failure this end produced rather than read off the
	// wire, which §13.1 closes with 3111.
	localFault atomic.Bool

	marshaler messageWriter
	// openOnce writes the M message that must precede every other message on
	// the stream.
	openOnce    sync.Once
	openErr     *connect.Error
	requestMeta http.Header
	unmarshaler websocketClientUnmarshaler

	responseHeader  http.Header
	responseTrailer http.Header

	responseHeaderOnce sync.Once
	sendCloseOnce      sync.Once
	sendCloseErr       *connect.Error
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
		c.openErr = c.marshaler.writeJSON(markerMetadata, encodeMetadata(c.requestMeta))
	})
	return c.openErr
}

func (c *websocketClientConn) Send(msg any) error {
	// The peer stops reading bodies once it has seen end-of-client-stream, so
	// this message would be discarded. connecthttp reports the same
	// mistake rather than letting the caller believe it was delivered.
	if c.sendClosed.Load() {
		return errorf(connect.CodeUnknown, "send after CloseSend: %w", io.EOF)
	}
	if err := c.open(); err != nil {
		return err
	}
	if err := c.marshaler.writeBody(msg); err != nil {
		c.noteLocalFault(err)
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

func (c *websocketClientConn) CloseRequest() error {
	// A stream that sent no message still has to open before it can end.
	if err := c.open(); err != nil {
		return err
	}
	c.sendCloseOnce.Do(func() {
		defer c.sendClosed.Store(true)
		// A bare C carries no payload to encode, so its frame type is the one
		// the subprotocol negotiated (§6.1) rather than the text a JSON payload
		// would have implied.
		c.sendCloseErr = c.marshaler.send(markerClientEndStream, c.marshaler.bodyIsText, nil)
	})
	if c.sendCloseErr != nil {
		return c.sendCloseErr
	}
	return nil
}

func (c *websocketClientConn) Receive(msg any) error {
	// A stream that only receives still has to open: the server will not
	// dispatch the handler until the opening metadata arrives.
	if err := c.open(); err != nil {
		return err
	}
	err := c.unmarshaler.Unmarshal(msg)
	if err == nil {
		return nil
	}
	// On EndStream, fold trailers and surface any server error. The trailers
	// stay on the conn: v2 carries them on CallInfo, which the transport
	// publishes, rather than attaching them to the error.
	if c.unmarshaler.endStreamSeen {
		mergeHeaders(c.responseTrailer, c.unmarshaler.trailer)
		if serverErr := c.unmarshaler.endStreamError; serverErr != nil {
			return serverErr
		}
	}
	return err
}

func (c *websocketClientConn) ResponseHeader() http.Header {
	// Leading-Metadata replaces same-key handshake values rather than appending
	// to them, so this runs after the 101's headers and assigns rather than
	// merging — which also keeps a second call from duplicating them.
	defer func() { maps.Copy(c.responseHeader, c.unmarshaler.header) }()
	c.responseHeaderOnce.Do(func() {
		if dialErr := c.call.ensureDialed(); dialErr != nil {
			return
		}
		if c.call.response == nil {
			return
		}
		for key, values := range c.call.response.Header {
			if isWebSocketProtocolHeader(key) {
				continue
			}
			c.responseHeader[key] = values
		}
	})
	return c.responseHeader
}

func (c *websocketClientConn) ResponseTrailer() http.Header {
	return c.responseTrailer
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
		c.localFault.Store(true)
	}
}

func (c *websocketClientConn) CloseResponse() error {
	if c.call.wsConn == nil {
		return nil
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
	return closeErr
}

func (c *websocketClientConn) closeConn() error {
	// A server that malformed or oversized its side of the stream gets hung up
	// on: the closing handshake reads until the peer's close frame arrives, so
	// extending it would mean draining whatever that server has queued. See the
	// note in websocketHandlerConn.Close.
	if c.unmarshaler.peerFaulted {
		// A client cannot send the 10xx code a server would: §13 puts its codes
		// in the 31xx range, the only one a browser script may use. A peer fault
		// is the more specific condition, so it wins over 3111 below.
		if !c.faultCloseCode {
			return c.call.wsConn.CloseNow()
		}
		return c.call.wsConn.Close(clientCloseCode(c.unmarshaler.fault), "")
	}
	if c.localFault.Load() {
		// §13.1: the RPC ended on an unexpected condition at this end, not on
		// anything the server did. Sent unconditionally — the peer behaved, so
		// the closing handshake has nothing queued to drain, and the ordinary
		// 1000 below costs the same handshake anyway.
		return c.call.wsConn.Close(wsStatusClientInternal, "")
	}
	// Best-effort clean close. Sending 1000 Normal Closure is correct even if
	// Receive has not yet observed the EndStream: the server's completion path
	// has already sent its own close, and Close is a no-op once the handshake
	// has happened.
	return c.call.wsConn.Close(websocket.StatusNormalClosure, "")
}

// websocketClientUnmarshaler reads one message per WebSocket frame. It mirrors
// websocketUnmarshaler (server-side) but recognizes the server's S message
// instead of the client's C and M.
type websocketClientUnmarshaler struct {
	call         *wsClientCall
	codecs       codecPair
	readMaxBytes int
	// bodyIsText mirrors the server's; see websocketUnmarshaler.
	bodyIsText bool
	// spec and onProtocolError identify and report a server that frames its
	// responses wrongly; see WithClientProtocolErrorHandler.
	spec            connect.Spec
	onProtocolError ClientProtocolErrorHandler
	fault           ProtocolFault

	endStreamSeen  bool
	peerFaulted    bool
	endStreamError *connect.Error
	trailer        http.Header
	header         http.Header
	// sawData gates the ordering rule on the response side: server metadata is
	// "leading" only while no body has arrived.
	sawData bool
	// sawLeadingMetadata records the server's opening M, which must arrive
	// exactly once and before anything else.
	sawLeadingMetadata bool
	// metadataConsumed reports that this frame was metadata, so Unmarshal reads
	// again rather than returning a message it never decoded.
	metadataConsumed bool
}

// Unmarshal records whether a failure was the peer's doing, which decides how
// the connection is torn down.
func (u *websocketClientUnmarshaler) Unmarshal(message any) *connect.Error {
	for {
		u.metadataConsumed = false
		u.fault = FaultUnknown
		err := u.unmarshal(message)
		if peerFault(err) {
			u.peerFaulted = true
		}
		// Gated on the classification rather than peerFault; see the server
		// side. Observation only — err is returned unchanged.
		if u.fault != FaultUnknown && u.onProtocolError != nil {
			u.onProtocolError(u.spec, u.fault, err)
		}
		if err == nil && u.metadataConsumed {
			continue
		}
		return err
	}
}

// fail records the kind of fault before building its error, so a monitor can
// tell them apart without matching message strings. See the server-side twin.
func (u *websocketClientUnmarshaler) fail(
	fault ProtocolFault,
	format string,
	args ...any,
) *connect.Error {
	u.fault = fault
	return errorf(connect.CodeInvalidArgument, format, args...)
}

// readFailure turns a failed read into the RPC's error. Every failure ends the
// response stream.
func (u *websocketClientUnmarshaler) readFailure(readerErr error) *connect.Error {
	u.endStreamSeen = true
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
	if limitErr := readLimitError(readerErr, u.readMaxBytes); limitErr != nil {
		// Classified like any other peer fault: a server sending more than
		// this client agreed to read is the server's mistake, and the
		// monitor should see it under the same name the server uses.
		u.fault = FaultSizeLimit
		return limitErr
	}
	return errorf(connect.CodeUnavailable, "read websocket message: %w", readerErr)
}

func (u *websocketClientUnmarshaler) unmarshal(message any) *connect.Error {
	if err := u.call.ensureDialed(); err != nil {
		u.endStreamSeen = true
		u.endStreamError = err
		return err
	}
	if u.endStreamSeen {
		return errorf(connect.CodeUnknown, "%w", io.EOF)
	}

	messageType, frame, readerErr := u.call.reader.next(u.call.ctx)
	if readerErr != nil {
		return u.readFailure(readerErr)
	}
	text := messageType == websocket.MessageText
	if !text && messageType != websocket.MessageBinary {
		return u.fail(FaultFrameType, "unknown WebSocket message type %d", messageType)
	}
	wire, decodeErr := decodeMessage(frame, text)
	if decodeErr != nil {
		u.fault = FaultMarker
		return decodeErr
	}
	if utf8Err := textEncodingError(frame, text); utf8Err != nil {
		u.fault = FaultMessageEncoding
		return utf8Err
	}
	if u.readMaxBytes > 0 && len(wire.payload) > u.readMaxBytes {
		u.fault = FaultSizeLimit
		return errorf(
			connect.CodeResourceExhausted,
			"message size %d is larger than configured max %d", len(wire.payload), u.readMaxBytes,
		)
	}

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
		u.fault = FaultMarker
		return errorf(
			connect.CodeInternal,
			"server sent %s, which only a client may send", markerName(wire.marker),
		)
	case markerMetadata:
		if mergeErr := u.mergeLeadingMetadata(wire.payload); mergeErr != nil {
			return mergeErr
		}
		u.metadataConsumed = true
		return nil
	case markerServerEndStream:
		// Parsed unconditionally, so a bare S fails here exactly as a
		// zero-length end-of-stream payload fails in Connect's HTTP streaming
		// protocol. The framing differs; the message does not.
		var end connectwire.EndStreamMessage
		if err := json.Unmarshal(wire.payload, &end); err != nil {
			u.fault = FaultMetadata
			return errorf(connect.CodeInternal, "unmarshal EndStreamMessage: %w", err)
		}
		trailer, decodeErr := decodeMetadata(end.Trailer)
		if decodeErr != nil {
			u.fault = FaultMetadata
			return decodeErr
		}
		u.endStreamSeen = true
		u.trailer = trailer
		u.endStreamError = end.Error.AsError()
		return errorf(connect.CodeUnknown, "%w", io.EOF)
	case markerBody:
		u.sawData = true
		if wire.text != u.bodyIsText {
			// The mirror of the server's check; see websocketUnmarshaler.
			return u.fail(
				FaultFrameType,
				"server sent a %s body on a %s connection",
				encodingForFrame(wire.text), encodingForFrame(u.bodyIsText),
			)
		}
		if len(wire.payload) == 0 {
			if wire.text {
				return u.fail(FaultFrameType, "empty body in a text frame; an empty JSON message is {}")
			}
			return nil
		}
		codec := u.codecs.forFrame(wire.text)
		if codec == nil {
			return u.fail(
				FaultFrameType,
				"server sent a %s body, which this client was not configured to decode",
				encodingForFrame(wire.text),
			)
		}
		if err := codec.UnmarshalRead(u.call.ctx, bytes.NewReader(wire.payload), message); err != nil {
			return u.fail(FaultMessageEncoding, "unmarshal message: %w", err)
		}
		return nil
	default:
		return u.fail(FaultMarker, "server sent unknown marker %s", markerName(wire.marker))
	}
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
	if len(payload) == 0 {
		u.fault = FaultMetadata
		return errorf(connect.CodeInternal, "empty M message; an empty metadata object is {}")
	}
	if dupErr := duplicateMetadataKey(payload); dupErr != nil {
		u.fault = FaultMetadata
		return dupErr
	}
	var wire map[string][]string
	if err := json.Unmarshal(payload, &wire); err != nil {
		u.fault = FaultMetadata
		return errorf(connect.CodeInternal, "unmarshal M message: %w", err)
	}
	meta, decodeErr := decodeMetadata(wire)
	if decodeErr != nil {
		u.fault = FaultMetadata
		return decodeErr
	}
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

func dialError(err error, response *http.Response) *connect.Error {
	switch {
	case errors.Is(err, context.Canceled):
		return errorf(connect.CodeCanceled, "WebSocket upgrade canceled: %w", err)
	case errors.Is(err, context.DeadlineExceeded):
		return errorf(connect.CodeDeadlineExceeded, "WebSocket upgrade deadline exceeded: %w", err)
	}
	if response != nil {
		return errorf(
			httpToCode(response.StatusCode),
			"WebSocket upgrade failed: HTTP %s: %w",
			response.Status, err,
		)
	}
	return errorf(connect.CodeUnavailable, "WebSocket upgrade failed: %w", err)
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
