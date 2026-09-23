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

// This transport's framing: one marker and one payload per WebSocket message,
// with no length field. The frame already delimits the message, so a length
// could only restate it — or disagree with it, which is a class of protocol
// error that cannot exist once the field is gone.
//
// The frame type carries the payload encoding: text is JSON, binary is
// Protobuf. A receiver therefore knows how to parse a payload before it has
// looked at anything but the frame.

package connectwebsocket

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/bufferpool"
	"connectrpc.com/connect/v2/internal/connecterr"
	"connectrpc.com/connect/v2/internal/connectwire"
)

// Message markers. Printable on purpose: a text frame reads as its marker
// followed by its JSON in a browser's network panel, which is where a JS
// client gets debugged.
const (
	markerBody            = 'B' // an RPC message, either direction
	markerMetadata        = 'M' // JSON http.Header, either direction
	markerServerEndStream = 'S' // EndStreamMessage JSON, server -> client
	markerClientEndStream = 'C' // end of the request stream, client -> server
)

// markerName renders a marker for a diagnostic, printably where it can.
func markerName(marker rune) string {
	if marker >= 0x20 && marker < 0x7F {
		return string(marker)
	}
	return "U+" + strconv.FormatInt(int64(marker), 16)
}

// encodeMessage writes marker and payload into dst as one message.
//
// Both go into a single buffer rather than two writes, because the WebSocket
// library decides whether to compress from the size of the first write it
// sees. Writing the marker alone would offer one byte, fall under any sane
// threshold, and silently disable compression for every message.
func encodeMessage(dst *bytes.Buffer, marker rune, payload []byte) {
	dst.Grow(utf8.RuneLen(marker) + len(payload))
	dst.WriteRune(marker)
	dst.Write(payload)
}

// decodeMessage splits a frame into its marker and payload.
func decodeMessage(frame []byte) (rune, []byte, *connect.Error) {
	if len(frame) == 0 {
		return 0, nil, errorf(connect.CodeInvalidArgument, "protocol error: empty message carries no marker")
	}
	// UTF-8's leading byte gives the length, so an astral marker is out of
	// range on sight: four-byte sequences start at 0xF0.
	if frame[0] >= 0xF0 {
		return 0, nil, errorf(
			connect.CodeInvalidArgument,
			"protocol error: marker outside the Basic Multilingual Plane (leading byte 0x%02x)",
			frame[0],
		)
	}
	marker, size := utf8.DecodeRune(frame)
	if marker == utf8.RuneError && size <= 1 {
		return 0, nil, errorf(connect.CodeInvalidArgument, "protocol error: marker is not valid UTF-8")
	}
	return marker, frame[size:], nil
}

// messageSender writes one encoded message as one WebSocket frame. text says
// which frame type to use.
type messageSender interface {
	send(text bool, data []byte) (int64, error)
}

// codecPair is the two codecs a connection may need. The frame type selects
// between them per message, so both are live on every connection even though
// most connections only ever use one.
type codecPair struct {
	binary connect.Codec // Protobuf
	text   connect.Codec // JSON
}

// forFrame returns the codec a frame of this type carries.
func (c codecPair) forFrame(text bool) connect.Codec {
	if text {
		return c.text
	}
	return c.binary
}

// messageWriter encodes messages and hands them to a sender.
type messageWriter struct {
	ctx    context.Context //nolint:containedctx // bound to the RPC, as the sender is
	sender messageSender
	codecs codecPair
	// bodyIsText is the negotiated default for outgoing bodies: JSON in text
	// frames, or Protobuf in binary ones. Control messages ignore it, being
	// always JSON.
	bodyIsText   bool
	sendMaxBytes int
	stats        *connect.MessageStats
}

// writeBody encodes message with the negotiated codec and sends it as a body.
func (w *messageWriter) writeBody(message any) *connect.Error {
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	if err := w.codecs.forFrame(w.bodyIsText).MarshalWrite(w.ctx, buffer, message); err != nil {
		return errorf(connect.CodeInternal, "marshal message: %w", err)
	}
	if err := w.send(markerBody, w.bodyIsText, buffer.Bytes()); err != nil {
		return err
	}
	if w.stats != nil {
		*w.stats = connect.MessageStats{Size: buffer.Len()}
	}
	return nil
}

// writeJSON sends a control message, whose payload is always JSON and whose
// frame is therefore always text.
func (w *messageWriter) writeJSON(marker rune, value any) *connect.Error {
	data, err := json.Marshal(value)
	if err != nil {
		return errorf(connect.CodeInternal, "marshal %s message: %w", markerName(marker), err)
	}
	return w.send(marker, true, data)
}

// writeEndStream sends the terminal message. Its EndStreamMessage schema is
// shared with Connect over HTTP; only the framing around it differs.
func (w *messageWriter) writeEndStream(err error, trailer http.Header) *connect.Error {
	end := &connectwire.EndStreamMessage{Trailer: trailer}
	if err != nil {
		end.Error = connectwire.NewWireError(err)
	}
	return w.writeJSON(markerServerEndStream, end)
}

// send encodes one message and hands it to the sender.
func (w *messageWriter) send(marker rune, text bool, payload []byte) *connect.Error {
	if w.sendMaxBytes > 0 && len(payload) > w.sendMaxBytes {
		return errorf(
			connect.CodeResourceExhausted,
			"message size %d exceeds sendMaxBytes %d", len(payload), w.sendMaxBytes,
		)
	}
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	encodeMessage(buffer, marker, payload)
	if _, sendErr := w.sender.send(text, buffer.Bytes()); sendErr != nil {
		wrapped := connecterr.WrapIfContextDone(w.ctx, sendErr)
		if connectErr, ok := connecterr.AsError(wrapped); ok {
			return connectErr
		}
		return errorf(connect.CodeUnknown, "write message: %w", wrapped)
	}
	return nil
}

// newCodecPair resolves the two codecs the frame type selects between. Both
// must be present: a peer may answer in either, whatever this end negotiated.
func newCodecPair(codecs []connect.Codec) (codecPair, *connect.Error) {
	registry := newCodecRegistry(codecs)
	binary, found := registry.get(connect.CodecNameProto)
	if !found {
		return codecPair{}, errorf(
			connect.CodeUnknown,
			"no %q codec registered; binary frames carry Protobuf", connect.CodecNameProto,
		)
	}
	text, found := registry.get(connect.CodecNameJSON)
	if !found {
		return codecPair{}, errorf(
			connect.CodeUnknown,
			"no %q codec registered; text frames carry JSON", connect.CodecNameJSON,
		)
	}
	return codecPair{binary: binary, text: text}, nil
}
