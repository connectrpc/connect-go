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

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/bufferpool"
	"connectrpc.com/connect/v2/internal/connectwire"
	"connectrpc.com/connect/v2/internal/envelope"
	"github.com/coder/websocket"
)

// Envelope framing over WebSocket frames, for both directions.
//
// The wire format reuses Connect's Enveloped-Message framing unchanged: each
// WebSocket binary message carries exactly one envelope, a 5-byte prefix plus
// its payload.
//
// Two envelope flag bits are client-only, because WebSocket lacks two things
// HTTP provides:
//
//   - Bit 2 (0x04) End-Of-Client-Stream — stands in for request-body EOF,
//     which a WebSocket cannot half-close. Carried by the client's final
//     envelope; the payload may be empty or the last data message.
//   - Bit 3 (0x08) Leading-Metadata — JSON-encoded http.Header, for browsers,
//     whose WebSocket API cannot set headers on the upgrade.
//
// Server-to-client framing is identical to Connect HTTP streaming: zero or
// more data envelopes, then an EndStream envelope (bit 1) whose payload is the
// EndStreamMessage JSON. [connectwire.StreamingMarshaler] produces it.

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

	// Client-only envelope flag bits. See the file comment for why each exists.
	wsFlagEnvelopeEndClientStream = 0b00000100 // bit 2
	wsFlagEnvelopeLeadingMetadata = 0b00001000 // bit 3

	// wsCloseWriteTimeout bounds the terminal write, which runs detached from
	// the RPC's deadline so that an expired deadline can still be reported.
	wsCloseWriteTimeout = 5 * time.Second

	// wsDrainLimit bounds how much of a malformed frame we are willing to throw
	// away. It mirrors the envelope package's discardLimit: a peer must not be
	// able to make us read forever just by appending garbage.
	wsDrainLimit = 1024 * 1024 * 4 // 4MiB

	// wsEnvelopePrefixBytes is the flags byte plus the uint32 length that
	// precede every enveloped message.
	wsEnvelopePrefixBytes = 5
)

// envelopeCompressionUnsupported explains a compressed-envelope flag arriving
// on a transport that compresses whole messages itself.
const envelopeCompressionUnsupported = "protocol error: envelope compression is not used over WebSocket; " +
	"the transport negotiates permessage-deflate instead"

// websocketHandlerConn is the server's view of one RPC. [session.Serve] wraps
// it in a [connect.ServerStream] and hands that to [connect.Server.Call].
type websocketHandlerConn struct {
	request *http.Request
	wsConn  *websocket.Conn

	marshaler   connectwire.StreamingMarshaler
	unmarshaler websocketUnmarshaler

	// callInfo is read at flush time rather than copied in: a handler sets its
	// leading metadata during the call, and the conn sends it before the first
	// message, so there is no point at which a snapshot would be current.
	callInfo        *connect.CallInfo
	responseTrailer http.Header
	leadingSent     bool
	// closeCtx writes the terminal envelopes. Data messages go out under the
	// RPC's context; the EndStream envelope cannot, because the commonest
	// reason to send one is that the context just expired.
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
	if err := c.marshaler.Marshal(msg); err != nil {
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

// terminalMarshaler copies the marshaler onto the detached close context, so
// the final envelopes are not written under a context whose expiry is the very
// thing being reported.
func (c *websocketHandlerConn) terminalMarshaler() connectwire.StreamingMarshaler {
	terminal := c.marshaler
	terminal.Ctx = c.closeCtx
	terminal.Sender = &websocketBinarySender{ctx: c.closeCtx, conn: c.wsConn}
	return terminal
}

// flushLeadingMetadata writes the Leading-Metadata envelope. It must run
// before the first data envelope and before the EndStream envelope alike: a
// stream that fails without sending a message is where leading metadata is
// most wanted, and there would be nothing else to carry it.
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
	data, marshalErr := json.Marshal(header)
	if marshalErr != nil {
		return errorf(connect.CodeInternal, "marshal Leading-Metadata: %w", marshalErr)
	}
	raw := bytes.NewBuffer(data)
	defer bufferpool.Put(raw)
	return c.marshaler.Write(&envelope.Envelope{
		Data:  raw,
		Flags: wsFlagEnvelopeLeadingMetadata,
	})
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
	// Leading metadata precedes the EndStream envelope even when no message was
	// sent, so headers stay headers rather than folding into the trailers.
	marshalErr := c.flushLeadingMetadata()
	if marshalErr == nil {
		marshalErr = c.marshaler.MarshalEndStream(err, c.responseTrailer)
	}
	closeCode := websocket.StatusNormalClosure
	var closeMessage string
	if marshalErr != nil {
		closeCode = websocket.StatusInternalError
		closeMessage = marshalErr.Message()
	}
	// The peer already has the verdict from the EndStream envelope above, so
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

// websocketBinarySender is the [envelope.MessageSender] for the server: it
// writes each envelope as one WebSocket binary message rather than appending
// it to an HTTP response body.
type websocketBinarySender struct {
	ctx  context.Context //nolint:containedctx
	conn *websocket.Conn
}

func (s *websocketBinarySender) Send(payload envelope.MessagePayload) (int64, error) {
	return writeMessage(s.ctx, s.conn, payload)
}

// writeMessage sends one envelope as a single WebSocket binary message.
//
// The envelope is assembled into one buffer rather than streamed into a
// message writer, because the library decides whether to compress from the
// size of the *first* write. An envelope streams as a 5-byte prefix followed
// by its payload, so streaming would offer 5 bytes, fall under any sane
// threshold, and silently disable compression for every message regardless of
// its real size.
func writeMessage(
	ctx context.Context,
	conn *websocket.Conn,
	payload envelope.MessagePayload,
) (int64, error) {
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	buffer.Grow(payload.Len())
	wroteN, err := payload.WriteTo(buffer)
	if err != nil {
		return wroteN, err
	}
	return wroteN, conn.Write(ctx, websocket.MessageBinary, buffer.Bytes())
}

// websocketUnmarshaler reads one envelope per WebSocket binary frame and
// transparently handles bit 2 (End-Of-Client-Stream) and bit 3
// (Leading-Metadata) before decoding data envelopes.
//
// Each frame gets its own [envelope.Reader], scoped to that frame, so no
// read state carries between frames.
type websocketUnmarshaler struct {
	ctx    context.Context //nolint:containedctx
	wsConn *websocket.Conn
	// callInfo receives Leading-Metadata envelopes. They arrive after the
	// handshake, so this is the only way a browser's metadata reaches the
	// handler.
	callInfo     *connect.CallInfo
	codec        connect.Codec
	readMaxBytes int
	// info is carried so a fault can be attributed to the connection that
	// produced it; its OnProtocolError is the monitor.
	info SessionInfo

	// fault classifies the most recent peer fault for OnProtocolError.
	fault       ProtocolFault
	eof         bool
	peerFaulted bool
	// sawData gates the ordering rule: metadata is "leading" only while no
	// data envelope has arrived.
	sawData bool
}

// Unmarshal records whether a failure was the peer's doing, which decides how
// the connection is torn down.
func (u *websocketUnmarshaler) Unmarshal(message any) *connect.Error {
	u.fault = FaultUnknown
	err := u.unmarshal(message)
	if peerFault(err) {
		u.peerFaulted = true
	}
	// Reporting is gated on the classification, not on peerFault: that
	// predicate decides how to tear the connection down, and some framing
	// faults are reported to the caller as Internal rather than as the peer's
	// fault. Observation only — err is returned unchanged.
	if u.fault != FaultUnknown && u.info.OnProtocolError != nil {
		u.info.OnProtocolError(u.info, u.fault, err)
	}
	return err
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
	for {
		messageType, frame, readerErr := u.wsConn.Reader(u.ctx)
		if readerErr != nil {
			u.eof = true
			if isCleanWebSocketClose(readerErr) {
				return errorf(connect.CodeUnknown, "%w", io.EOF)
			}
			if limitErr := readLimitError(readerErr); limitErr != nil {
				return limitErr
			}
			// The client vanished before signalling end-of-stream. Canceled
			// rather than Unavailable: the peer stopped, the transport did not
			// fail, and a caller should not retry on the client's behalf.
			return errorf(connect.CodeCanceled, "websocket closed before end-of-stream: %w", readerErr)
		}
		if messageType != websocket.MessageBinary {
			drainFrame(frame)
			return u.fail(
				FaultFrameType,
				"Connect over WebSocket requires binary frames; got message type %d",
				messageType,
			)
		}

		buffer := bufferpool.Get()
		env := &envelope.Envelope{Data: buffer}
		// Scoped to this frame: Read tracks BytesRead, which must not carry
		// over to the next one.
		reader := &envelope.Reader{
			Ctx:          u.ctx,
			Src:          frame,
			ReadMaxBytes: u.readMaxBytes,
		}
		if readErr := reader.Read(env); readErr != nil {
			bufferpool.Put(buffer)
			if limitErr := readLimitError(readErr); limitErr != nil {
				u.fault = FaultSizeLimit
				return limitErr
			}
			// Anything else from a single-envelope frame is a length that did
			// not match it: the reader ran off the end of the frame.
			u.fault = FaultEnvelopeLength
			if readErr.Code() == connect.CodeResourceExhausted {
				u.fault = FaultSizeLimit
			}
			return readErr
		}
		// Each binary frame must contain exactly one envelope.
		if extra := drainFrame(frame); extra > 0 {
			bufferpool.Put(buffer)
			return u.fail(
				FaultEnvelopeLength,
				"websocket frame contains %d or more extra bytes after envelope",
				extra,
			)
		}

		flags := env.Flags
		switch {
		case flags&wsFlagEnvelopeLeadingMetadata != 0:
			if u.sawData {
				bufferpool.Put(buffer)
				return u.fail(
					FaultMetadata,
					"client sent Leading-Metadata after a message; metadata is leading only before the first one",
				)
			}
			mergeErr := u.mergeLeadingMetadata(env)
			bufferpool.Put(buffer)
			if mergeErr != nil {
				return mergeErr
			}
			continue
		case flags&wsFlagEnvelopeEndClientStream != 0:
			// Final envelope from the client. If it carries a payload, deliver
			// it as a normal message; mark EOF either way so the next Receive
			// call returns io.EOF.
			u.eof = true
			u.sawData = true
			if env.Data.Len() == 0 {
				bufferpool.Put(buffer)
				return errorf(connect.CodeUnknown, "%w", io.EOF)
			}
			decodeErr := u.decodeData(env, message)
			bufferpool.Put(buffer)
			return decodeErr
		case flags == 0 || flags == envelope.FlagCompressed:
			u.sawData = true
			decodeErr := u.decodeData(env, message)
			bufferpool.Put(buffer)
			return decodeErr
		case flags&connectwire.FlagEnvelopeEndStream != 0:
			bufferpool.Put(buffer)
			return u.fail(
				FaultEnvelopeFlags,
				"client sent envelope with server-only flags: 0x%02x", flags,
			)
		default:
			bufferpool.Put(buffer)
			return u.fail(
				FaultEnvelopeFlags,
				"client sent envelope with reserved flags: 0x%02x", flags,
			)
		}
	}
}

func (u *websocketUnmarshaler) decodeData(env *envelope.Envelope, message any) *connect.Error {
	data := env.Data
	if env.IsSet(envelope.FlagCompressed) {
		return u.fail(FaultEnvelopeFlags, "%s", envelopeCompressionUnsupported)
	}
	if data.Len() == 0 {
		// Zero value of the message is correct.
		return nil
	}
	if err := u.codec.UnmarshalRead(u.ctx, data, message); err != nil {
		return u.fail(FaultMessageEncoding, "unmarshal message: %w", err)
	}
	return nil
}

func (u *websocketUnmarshaler) mergeLeadingMetadata(env *envelope.Envelope) *connect.Error {
	data := env.Data
	if env.IsSet(envelope.FlagCompressed) {
		return u.fail(FaultEnvelopeFlags, "%s", envelopeCompressionUnsupported)
	}
	if data.Len() == 0 {
		return nil
	}
	var meta map[string][]string
	if err := json.Unmarshal(data.Bytes(), &meta); err != nil {
		return u.fail(FaultMetadata, "unmarshal Leading-Metadata envelope: %w", err)
	}
	// Envelope metadata wins over the upgrade headers: it is the only channel a
	// browser has, and it arrives later, so it is the more specific statement.
	for key, values := range meta {
		u.callInfo.RequestHeader().SetValues(http.CanonicalHeaderKey(key), values)
	}
	return nil
}

// Helpers.

// frameReadLimit turns a per-message limit into a per-frame one. A legal frame
// carries exactly one envelope, so it is the prefix plus the payload.
//
// The result is always passed to SetReadLimit, never skipped: coder's default
// is 32KiB, and a zero would cap frames at a single byte. Unlimited is -1.
func frameReadLimit(readMaxBytes int) int64 {
	if readMaxBytes <= 0 {
		return -1
	}
	return int64(readMaxBytes) + wsEnvelopePrefixBytes
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

// drainFrame discards what is left of a frame, giving up after wsDrainLimit
// bytes. The count it returns is therefore a lower bound.
func drainFrame(frame io.Reader) int64 {
	limited := &io.LimitedReader{R: frame, N: wsDrainLimit}
	discarded, _ := io.Copy(io.Discard, limited)
	return discarded
}

func isCleanWebSocketClose(err error) bool {
	// Not a switch: the repo's exhaustive linter wants every StatusCode listed,
	// and enumerating thirteen irrelevant close codes would obscure the rule.
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}

// The client mirrors the server: one envelope per binary frame, with
// [envelope.Writer] marshaling outbound and websocketClientUnmarshaler reading
// inbound and recognizing the server's bit-1 EndStream envelope.

// subprotocolForCodec maps a codec to the token that names it. The client
// always offers the explicit token, so the server's codec choice cannot drift
// from the one the client is encoding with.
func subprotocolForCodec(name string) string {
	if name == connect.CodecNameJSON {
		return wsSubprotocolJSON
	}
	return wsSubprotocolProto
}

// wsClientCall owns the lazy dial and is the [envelope.MessageSender] the
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

// Send implements [envelope.MessageSender], writing one envelope as a single
// WebSocket binary message.
func (c *wsClientCall) Send(payload envelope.MessagePayload) (int64, error) {
	if err := c.ensureDialed(); err != nil {
		return 0, err
	}
	return writeMessage(c.ctx, c.wsConn, payload)
}

type websocketClientConn struct {
	call  *wsClientCall
	codec connect.Codec

	marshaler   envelope.Writer
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

func (c *websocketClientConn) Send(msg any) error {
	// The peer stops reading data envelopes once it has seen end-of-client-
	// stream, so this message would be discarded. connecthttp reports the same
	// mistake rather than letting the caller believe it was delivered.
	if c.sendClosed.Load() {
		return errorf(connect.CodeUnknown, "send after CloseSend: %w", io.EOF)
	}
	if err := c.marshaler.Marshal(msg); err != nil {
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

func (c *websocketClientConn) CloseRequest() error {
	c.sendCloseOnce.Do(func() {
		defer c.sendClosed.Store(true)
		// Send an empty bit-2 envelope to signal End-Of-Client-Stream.
		emptyBuffer := bufferpool.Get()
		defer bufferpool.Put(emptyBuffer)
		c.sendCloseErr = c.marshaler.Write(&envelope.Envelope{
			Data:  emptyBuffer,
			Flags: wsFlagEnvelopeEndClientStream,
		})
	})
	if c.sendCloseErr != nil {
		return c.sendCloseErr
	}
	return nil
}

func (c *websocketClientConn) Receive(msg any) error {
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

// websocketClientUnmarshaler reads one envelope per WebSocket binary frame.
// It mirrors websocketUnmarshaler (server-side) but recognizes the server's
// bit-1 EndStream envelope instead of the client's bit-2/bit-3 flags.
type websocketClientUnmarshaler struct {
	call         *wsClientCall
	codec        connect.Codec
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
	// sawData gates the ordering rule: metadata is "leading" only while no
	// data envelope has arrived.
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

	messageType, frame, readerErr := u.call.wsConn.Reader(u.call.ctx)
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
	if messageType != websocket.MessageBinary {
		drainFrame(frame)
		return u.fail(
			FaultFrameType,
			"Connect over WebSocket requires binary frames; got message type %d",
			messageType,
		)
	}

	buffer := bufferpool.Get()
	env := &envelope.Envelope{Data: buffer}
	reader := &envelope.Reader{
		Ctx:          u.call.ctx,
		Src:          frame,
		Codec:        u.codec,
		ReadMaxBytes: u.readMaxBytes,
	}
	if readErr := reader.Read(env); readErr != nil {
		bufferpool.Put(buffer)
		if limitErr := readLimitError(readErr); limitErr != nil {
			u.fault = FaultSizeLimit
			return limitErr
		}
		u.fault = FaultEnvelopeLength
		if readErr.Code() == connect.CodeResourceExhausted {
			u.fault = FaultSizeLimit
		}
		return readErr
	}
	if extra := drainFrame(frame); extra > 0 {
		bufferpool.Put(buffer)
		return u.fail(
			FaultEnvelopeLength,
			"websocket frame contains %d or more extra bytes after envelope",
			extra,
		)
	}

	flags := env.Flags
	switch {
	case flags&wsFlagEnvelopeEndClientStream != 0:
		bufferpool.Put(buffer)
		u.fault = FaultEnvelopeFlags
		return errorf(
			connect.CodeInternal,
			"server sent envelope with client-only flags: 0x%02x", flags,
		)
	case flags&wsFlagEnvelopeLeadingMetadata != 0:
		mergeErr := u.mergeLeadingMetadata(env)
		bufferpool.Put(buffer)
		if mergeErr != nil {
			return mergeErr
		}
		u.metadataConsumed = true
		return nil
	case flags&connectwire.FlagEnvelopeEndStream != 0:
		defer bufferpool.Put(buffer)
		data := env.Data
		if env.IsSet(envelope.FlagCompressed) {
			return u.fail(FaultEnvelopeFlags, "%s", envelopeCompressionUnsupported)
		}
		var end connectwire.EndStreamMessage
		if data.Len() > 0 {
			if err := json.Unmarshal(data.Bytes(), &end); err != nil {
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
	case flags == 0 || flags == envelope.FlagCompressed:
		defer bufferpool.Put(buffer)
		u.sawData = true
		data := env.Data
		if env.IsSet(envelope.FlagCompressed) {
			return u.fail(FaultEnvelopeFlags, "%s", envelopeCompressionUnsupported)
		}
		if data.Len() == 0 {
			return nil
		}
		if err := u.codec.UnmarshalRead(u.call.ctx, data, message); err != nil {
			return u.fail(FaultMessageEncoding, "unmarshal message: %w", err)
		}
		return nil
	default:
		bufferpool.Put(buffer)
		return u.fail(
			FaultEnvelopeFlags,
			"server sent envelope with reserved flags: 0x%02x", flags,
		)
	}
}

// mergeLeadingMetadata folds a server Leading-Metadata envelope into the
// response headers. Later envelopes replace same-key values rather than
// appending, matching the client-to-server direction.
func (u *websocketClientUnmarshaler) mergeLeadingMetadata(env *envelope.Envelope) *connect.Error {
	if env.IsSet(envelope.FlagCompressed) {
		return errorf(connect.CodeInvalidArgument, "%s", envelopeCompressionUnsupported)
	}
	if u.sawData {
		return u.fail(
			FaultMetadata,
			"server sent Leading-Metadata after a message; metadata is leading only before the first one",
		)
	}
	data := env.Data
	if data.Len() == 0 {
		return nil
	}
	var meta map[string][]string
	if err := json.Unmarshal(data.Bytes(), &meta); err != nil {
		u.fault = FaultMetadata
		return errorf(connect.CodeInternal, "unmarshal Leading-Metadata envelope: %w", err)
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
