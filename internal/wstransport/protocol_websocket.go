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
	"errors"
	"io"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
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

// wsContextError maps a done context's error to the matching Connect code,
// preserving the original error via %w.
func wsContextError(ctxErr error) *connect.Error {
	if errors.Is(ctxErr, context.Canceled) {
		return errorf(connect.CodeCanceled, "%w", ctxErr)
	}
	return errorf(connect.CodeDeadlineExceeded, "%w", ctxErr)
}
