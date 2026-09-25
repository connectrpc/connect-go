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
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/bufferpool"
	"connectrpc.com/connect/v2/internal/connecterr"
	"connectrpc.com/connect/v2/internal/connectwire"
)

// Message markers: one byte each, printable on purpose. A text frame then
// reads as its marker followed by its JSON in a browser's network panel, which
// is where a JS client gets debugged.
const (
	markerBody            byte = 'B' // an RPC message, either direction
	markerMetadata        byte = 'M' // JSON http.Header, either direction
	markerServerEndStream byte = 'S' // EndStreamMessage JSON, server -> client
	markerClientEndStream byte = 'C' // end of the request stream, client -> server

	// markerHighBit is reserved: every marker this revision defines is below
	// it, so a later revision has a signal no conforming peer can already be
	// emitting.
	markerHighBit byte = 0x80
)

// markerName renders a marker for a diagnostic, printably where it can.
func markerName(marker byte) string {
	if marker >= 0x20 && marker < 0x7F {
		return string(rune(marker))
	}
	return "0x" + strconv.FormatUint(uint64(marker), 16)
}

// encodeMessage writes marker and payload into dst as one message.
//
// Both go into a single buffer rather than two writes, because the WebSocket
// library decides whether to compress from the size of the first write it
// sees. Writing the marker alone would offer one byte, fall under any sane
// threshold, and silently disable compression for every message.
func encodeMessage(dst *bytes.Buffer, marker byte, payload []byte) {
	dst.Grow(1 + len(payload))
	dst.WriteByte(marker)
	dst.Write(payload)
}

// wireMessage is one message as it arrived: the marker that names its kind,
// the payload behind it, and the frame type that says how that payload is
// encoded.
//
// The three travel together from the read all the way to the codec, because
// none of them means anything without the others: a payload without its frame
// type has no encoding, and a marker without its payload has no content.
type wireMessage struct {
	marker  byte
	payload []byte
	// text reports a text frame, whose payload is JSON.
	text bool
}

// decodeMessage splits a frame into the message it carries. text is the frame
// type it arrived in, which the caller reads from the transport.
func decodeMessage(frame []byte, text bool) (wireMessage, *connect.Error) {
	if len(frame) == 0 {
		return wireMessage{}, errorf(connect.CodeInvalidArgument, "protocol error: empty message carries no marker")
	}
	// The high bit is reserved for a later revision, so a byte that sets it is
	// refused before anything behind it is interpreted.
	if frame[0] >= markerHighBit {
		return wireMessage{}, errorf(
			connect.CodeInvalidArgument,
			"protocol error: marker 0x%02x sets the reserved high bit", frame[0],
		)
	}
	return wireMessage{marker: frame[0], payload: frame[1:], text: text}, nil
}

// messageSender writes one encoded message as one WebSocket frame. text says
// which frame type to use.
type messageSender interface {
	send(text bool, data []byte) (int64, error)
}

// codecPair is the two codecs a connection may need. The frame type selects
// between them per message, so both are live on every connection even though
// most connections only ever use one.
//
// Either may be absent: a peer configured for one encoding is a supported
// configuration, and negotiation never hands it a subprotocol it cannot serve.
// A body arriving in the other frame type anyway is a peer that ignored the
// negotiated subprotocol, and is reported rather than dereferenced.
type codecPair struct {
	binary connect.Codec // Protobuf
	text   connect.Codec // JSON
}

// forFrame returns the codec a frame of this type carries, or nil when this
// end was not configured with it.
func (c codecPair) forFrame(text bool) connect.Codec {
	if text {
		return c.text
	}
	return c.binary
}

// encodingForFrame names the encoding a frame type carries, for a diagnostic.
func encodingForFrame(text bool) string {
	if text {
		return connect.CodecNameJSON
	}
	return connect.CodecNameProto
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
	codec := w.codecs.forFrame(w.bodyIsText)
	if codec == nil {
		return errorf(
			connect.CodeInternal,
			"no %q codec to encode this body", encodingForFrame(w.bodyIsText),
		)
	}
	if err := codec.MarshalWrite(w.ctx, buffer, message); err != nil {
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
func (w *messageWriter) writeJSON(marker byte, value any) *connect.Error {
	data, err := json.Marshal(value)
	if err != nil {
		return errorf(connect.CodeInternal, "marshal %s message: %w", markerName(marker), err)
	}
	return w.send(marker, true, data)
}

// writeEndStream sends the terminal message. Its EndStreamMessage schema is
// shared with Connect over HTTP; only the framing around it differs.
func (w *messageWriter) writeEndStream(err error, trailer http.Header) *connect.Error {
	end := &connectwire.EndStreamMessage{Trailer: encodeMetadata(trailer)}
	if err != nil {
		end.Error = connectwire.NewWireError(err)
	}
	return w.writeJSON(markerServerEndStream, end)
}

// send encodes one message and hands it to the sender.
func (w *messageWriter) send(marker byte, text bool, payload []byte) *connect.Error {
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

// newCodecPair resolves the codecs the frame type selects between. Either may
// be missing — one encoding is a legitimate configuration — but not both, which
// leaves no body this end could decode and no subprotocol it could negotiate.
func newCodecPair(codecs []connect.Codec) (codecPair, *connect.Error) {
	registry := newCodecRegistry(codecs)
	binary, _ := registry.get(connect.CodecNameProto)
	text, _ := registry.get(connect.CodecNameJSON)
	if binary == nil && text == nil {
		return codecPair{}, errorf(
			connect.CodeUnknown,
			"no %q or %q codec registered; WebSocket bodies carry one or the other",
			connect.CodecNameProto, connect.CodecNameJSON,
		)
	}
	return codecPair{binary: binary, text: text}, nil
}

// Metadata on the wire is a JSON object of string arrays. Two things separate
// it from the http.Header this package keeps in memory: keys are lower-case,
// matching HTTP/2 and HTTP/3, and a key ending in -bin carries base64 rather
// than text, because JSON strings are Unicode and arbitrary bytes are not.

// metadataBinarySuffix marks a key whose values are raw bytes.
const metadataBinarySuffix = "-bin"

// encodeMetadata renders headers for the wire.
func encodeMetadata(header http.Header) map[string][]string {
	if len(header) == 0 {
		return map[string][]string{}
	}
	encoded := make(map[string][]string, len(header))
	for key, values := range header {
		lower := strings.ToLower(key)
		if !strings.HasSuffix(lower, metadataBinarySuffix) {
			encoded[lower] = values
			continue
		}
		binary := make([]string, len(values))
		for index, value := range values {
			binary[index] = base64.RawStdEncoding.EncodeToString([]byte(value))
		}
		encoded[lower] = binary
	}
	return encoded
}

// decodeMetadata reads wire metadata into an http.Header. Keys are canonical
// MIME form in memory so that Go's own case-insensitive accessors work; the
// case-insensitivity the protocol requires is theirs.
func decodeMetadata(wire map[string][]string) (http.Header, *connect.Error) {
	header := make(http.Header, len(wire))
	for key, values := range wire {
		canonical := http.CanonicalHeaderKey(key)
		if !strings.HasSuffix(strings.ToLower(key), metadataBinarySuffix) {
			header[canonical] = values
			continue
		}
		decoded := make([]string, len(values))
		for index, value := range values {
			raw, err := base64.RawStdEncoding.DecodeString(value)
			if err != nil {
				return nil, errorf(
					connect.CodeInvalidArgument,
					"metadata %q is not unpadded base64: %w", key, err,
				)
			}
			decoded[index] = string(raw)
		}
		header[canonical] = decoded
	}
	return header, nil
}
