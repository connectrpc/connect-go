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

package connectwebsocket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
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
// Each WebSocket frame carries exactly one message: a single BMP Unicode
// scalar naming its kind, then the payload. There is no length prefix — the
// frame is the boundary — and the frame type says how the payload is encoded,
// binary for Protobuf and text for JSON.
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

	wsSubprotocolBase  = "connect.v2"
	wsSubprotocolProto = "connect.v2+proto"
	wsSubprotocolJSON  = "connect.v2+json"

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
func (c *websocketHandlerConn) flushLeadingMetadata() *connect.Error {
	if c.leadingSent {
		return nil
	}
	c.leadingSent = true
	if c.callInfo == nil {
		return nil
	}
	header := make(map[string][]string)
	for key, values := range c.callInfo.ResponseHeader().All() {
		header[http.CanonicalHeaderKey(key)] = values
	}
	if len(header) == 0 {
		return nil
	}
	return c.marshaler.writeJSON(markerMetadata, header)
}

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

// closeReason truncates to the 125 bytes a WebSocket close frame allows.
func closeReason(reason string) string {
	const maxCloseReasonBytes = 125
	if len(reason) <= maxCloseReasonBytes {
		return reason
	}
	return reason[:maxCloseReasonBytes]
}

func (c *websocketHandlerConn) Close(err error) error {
	// A deadline that fired is what produced most of the errors reaching here,
	// and writing under the expired context would fail — leaving the peer with
	// a closed connection and no verdict, which it can only report as
	// Unavailable. Detach for the final write.
	if c.closeCtx != nil {
		c.marshaler = c.terminalMarshaler()
	}
	// Leading metadata precedes the S message even when no body was
	// sent, so headers stay headers rather than folding into the trailers.
	marshalErr := c.flushLeadingMetadata()
	if marshalErr == nil {
		marshalErr = c.marshaler.writeEndStream(err, c.responseTrailer)
	}
	closeCode := websocket.StatusNormalClosure
	var closeMessage string
	if marshalErr != nil {
		closeCode = websocket.StatusInternalError
		closeMessage = marshalErr.Message()
	}
	// The peer already has the verdict from the S message above, so
	// the close frame is courtesy. Extend it only to peers that behaved.
	//
	// The closing handshake reads until the peer's close frame arrives, which
	// means draining whatever it has queued. A peer that just overran the read
	// limit can exploit that: measured, a bomb rejected after 96KiB of reading
	// still cost 6.4MiB once the handshake drained the rest. CloseNow skips the
	// handshake and hangs up.
	if c.unmarshaler.peerFaulted {
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
	wsConn *websocket.Conn
	// callInfo receives M messages. They arrive after the
	// handshake, so this is the only way a browser's metadata reaches the
	// handler.
	callInfo     *connect.CallInfo
	codecs       codecPair
	readMaxBytes int
	// info is carried so a fault can be attributed to the connection that
	// produced it; its OnProtocolError is the monitor.
	info SessionInfo

	// fault classifies the most recent peer fault for OnProtocolError.
	fault       ProtocolFault
	eof         bool
	peerFaulted bool
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
		return errorf(connect.CodeUnknown, "%w", io.EOF)
	}
	marker, payload, text, readErr := u.nextMessage()
	if readErr != nil {
		return readErr
	}
	return u.dispatch(marker, payload, text, message)
}

// nextMessage reads one frame and returns the message it carries.
func (u *websocketUnmarshaler) nextMessage() (rune, []byte, bool, *connect.Error) {
	messageType, frame, readerErr := u.wsConn.Read(u.ctx)
	if readerErr != nil {
		u.eof = true
		if isCleanWebSocketClose(readerErr) {
			return 0, nil, false, errorf(connect.CodeUnknown, "%w", io.EOF)
		}
		if limitErr := readLimitError(readerErr); limitErr != nil {
			u.fault = FaultSizeLimit
			return 0, nil, false, limitErr
		}
		// The client vanished before signalling end-of-stream. Canceled
		// rather than Unavailable: the peer stopped, the transport did not
		// fail, and a caller should not retry on the client's behalf.
		return 0, nil, false, errorf(connect.CodeCanceled, "websocket closed before end-of-stream: %w", readerErr)
	}
	text := messageType == websocket.MessageText
	if !text && messageType != websocket.MessageBinary {
		return 0, nil, false, u.fail(FaultFrameType, "unknown WebSocket message type %d", messageType)
	}
	marker, payload, decodeErr := decodeMessage(frame)
	if decodeErr != nil {
		u.fault = FaultMarker
		return 0, nil, false, decodeErr
	}
	if len(payload) > u.readMaxBytes && u.readMaxBytes > 0 {
		u.fault = FaultSizeLimit
		return 0, nil, false, errorf(
			connect.CodeResourceExhausted,
			"message size %d is larger than configured max %d", len(payload), u.readMaxBytes,
		)
	}
	return marker, payload, text, nil
}

// dispatch interprets one message and decodes its payload into message.
func (u *websocketUnmarshaler) dispatch(
	marker rune,
	payload []byte,
	text bool,
	message any,
) *connect.Error {
	switch marker {
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
		if len(payload) == 0 {
			return errorf(connect.CodeUnknown, "%w", io.EOF)
		}
		return u.decodeBody(payload, text, message)
	case markerBody:
		return u.decodeBody(payload, text, message)
	case markerServerEndStream:
		return u.fail(
			FaultMarker,
			"client sent %s, which only a server may send", markerName(marker),
		)
	default:
		return u.fail(FaultMarker, "client sent unknown marker %s", markerName(marker))
	}
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
	marker, payload, _, readErr := u.nextMessage()
	if readErr != nil {
		u.reportFault(readErr)
		return readErr
	}
	if marker != markerMetadata {
		// Anything before the opening metadata means the peer is not speaking
		// this protocol; there is no partial state worth keeping.
		failure := u.fail(
			FaultMetadata,
			"client's first message must be M; got %s", markerName(marker),
		)
		u.reportFault(failure)
		return failure
	}
	mergeErr := u.mergeLeadingMetadata(payload)
	if mergeErr != nil {
		u.reportFault(mergeErr)
	}
	return mergeErr
}

// decodeBody decodes a body payload with the codec its frame type names.
func (u *websocketUnmarshaler) decodeBody(payload []byte, text bool, message any) *connect.Error {
	if len(payload) == 0 {
		if text {
			// An empty JSON body is "{}", never zero bytes, so a text frame
			// carrying only a marker says nothing the codec could decode.
			return u.fail(FaultFrameType, "empty body in a text frame; an empty JSON message is {}")
		}
		// The empty Protobuf message: the zero value is correct.
		return nil
	}
	codec := u.codecs.forFrame(text)
	if err := codec.UnmarshalRead(u.ctx, bytes.NewReader(payload), message); err != nil {
		return u.fail(FaultMessageEncoding, "unmarshal message: %w", err)
	}
	return nil
}

func (u *websocketUnmarshaler) mergeLeadingMetadata(payload []byte) *connect.Error {
	if len(payload) == 0 {
		return nil
	}
	var meta map[string][]string
	if err := json.Unmarshal(payload, &meta); err != nil {
		return u.fail(FaultMetadata, "unmarshal M message: %w", err)
	}
	// M metadata wins over the upgrade headers: it is the only channel a
	// browser has, and it arrives later, so it is the more specific statement.
	for key, values := range meta {
		u.callInfo.RequestHeader().SetValues(http.CanonicalHeaderKey(key), values)
	}
	return nil
}

// Helpers.

// frameReadLimit turns a per-message limit into a per-frame one. A legal frame
// carries exactly one message, so it is the marker plus the payload.
//
// The result is always passed to SetReadLimit, never skipped: coder's default
// is 32KiB, and a zero would cap frames at a single byte. Unlimited is -1.
func frameReadLimit(readMaxBytes int) int64 {
	if readMaxBytes <= 0 {
		return -1
	}
	// The marker is at most four bytes; the payload limit is the rest.
	return int64(readMaxBytes) + utf8.UTFMax
}

// readLimitError recognizes a message that blew a read limit, whether this
// side enforced it or the peer closed the connection because of it. Both mean
// the same thing to a caller, and neither is a transport failure.
func readLimitError(err error) *connect.Error {
	if websocket.CloseStatus(err) == websocket.StatusMessageTooBig {
		return errorf(connect.CodeResourceExhausted, "peer rejected message as too big: %w", err)
	}
	if errors.Is(err, websocket.ErrMessageTooBig) {
		return errorf(connect.CodeResourceExhausted, "message exceeds read limit: %w", err)
	}
	return nil
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

	dialOnce sync.Once
	dialDone chan struct{}
	wsConn   *websocket.Conn
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
		if got := conn.Subprotocol(); got != c.subprotocol {
			_ = conn.Close(websocket.StatusProtocolError, "unexpected subprotocol")
			c.dialErr = errorf(
				connect.CodeInternal,
				"server selected unexpected Sec-WebSocket-Protocol %q (want %q)",
				got, c.subprotocol,
			)
			return
		}
		// Bounds the inflated message and the compressed input both; see the
		// note in session.Serve.
		conn.SetReadLimit(c.readLimit)
		c.wsConn = conn
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
		metadata := c.requestMeta
		if metadata == nil {
			metadata = http.Header{}
		}
		c.openErr = c.marshaler.writeJSON(markerMetadata, metadata)
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
		// A bare C message: no payload, so a text frame per the framing rule.
		c.sendCloseErr = c.marshaler.send(markerClientEndStream, true, nil)
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

func (c *websocketClientConn) CloseResponse() error {
	if c.call.wsConn == nil {
		return nil
	}
	// A server that malformed or oversized its side of the stream gets hung up
	// on: the closing handshake reads until the peer's close frame arrives, so
	// extending it would mean draining whatever that server has queued. See the
	// note in websocketHandlerConn.Close.
	if c.unmarshaler.peerFaulted {
		return c.call.wsConn.CloseNow()
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

func (u *websocketClientUnmarshaler) unmarshal(message any) *connect.Error {
	if err := u.call.ensureDialed(); err != nil {
		u.endStreamSeen = true
		u.endStreamError = err
		return err
	}
	if u.endStreamSeen {
		return errorf(connect.CodeUnknown, "%w", io.EOF)
	}

	messageType, frame, readerErr := u.call.wsConn.Read(u.call.ctx)
	if readerErr != nil {
		u.endStreamSeen = true
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
		if limitErr := readLimitError(readerErr); limitErr != nil {
			return limitErr
		}
		return errorf(connect.CodeUnavailable, "read websocket message: %w", readerErr)
	}
	text := messageType == websocket.MessageText
	if !text && messageType != websocket.MessageBinary {
		return u.fail(FaultFrameType, "unknown WebSocket message type %d", messageType)
	}
	marker, payload, decodeErr := decodeMessage(frame)
	if decodeErr != nil {
		u.fault = FaultMarker
		return decodeErr
	}
	if u.readMaxBytes > 0 && len(payload) > u.readMaxBytes {
		u.fault = FaultSizeLimit
		return errorf(
			connect.CodeResourceExhausted,
			"message size %d is larger than configured max %d", len(payload), u.readMaxBytes,
		)
	}

	switch marker {
	case markerClientEndStream:
		u.fault = FaultMarker
		return errorf(
			connect.CodeInternal,
			"server sent %s, which only a client may send", markerName(marker),
		)
	case markerMetadata:
		if mergeErr := u.mergeLeadingMetadata(payload); mergeErr != nil {
			return mergeErr
		}
		u.metadataConsumed = true
		return nil
	case markerServerEndStream:
		var end connectwire.EndStreamMessage
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &end); err != nil {
				u.fault = FaultMetadata
				return errorf(connect.CodeInternal, "unmarshal EndStreamMessage: %w", err)
			}
		}
		for name, value := range end.Trailer {
			canonical := http.CanonicalHeaderKey(name)
			if name != canonical {
				delete(end.Trailer, name)
				end.Trailer[canonical] = append(end.Trailer[canonical], value...)
			}
		}
		u.endStreamSeen = true
		u.trailer = end.Trailer
		u.endStreamError = end.Error.AsError()
		return errorf(connect.CodeUnknown, "%w", io.EOF)
	case markerBody:
		u.sawData = true
		if len(payload) == 0 {
			if text {
				return u.fail(FaultFrameType, "empty body in a text frame; an empty JSON message is {}")
			}
			return nil
		}
		codec := u.codecs.forFrame(text)
		if err := codec.UnmarshalRead(u.call.ctx, bytes.NewReader(payload), message); err != nil {
			return u.fail(FaultMessageEncoding, "unmarshal message: %w", err)
		}
		return nil
	default:
		return u.fail(FaultMarker, "server sent unknown marker %s", markerName(marker))
	}
}

// mergeLeadingMetadata folds a server M message into the response headers.
// Later messages replace same-key values rather than appending, matching the
// client-to-server direction.
func (u *websocketClientUnmarshaler) mergeLeadingMetadata(payload []byte) *connect.Error {
	if u.sawData {
		return u.fail(
			FaultMetadata,
			"server sent M after a body; metadata is leading only before the first one",
		)
	}
	if len(payload) == 0 {
		return nil
	}
	var meta map[string][]string
	if err := json.Unmarshal(payload, &meta); err != nil {
		u.fault = FaultMetadata
		return errorf(connect.CodeInternal, "unmarshal M message: %w", err)
	}
	if u.header == nil {
		u.header = make(http.Header, len(meta))
	}
	for key, values := range meta {
		u.header[http.CanonicalHeaderKey(key)] = values
	}
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
