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

package wstransport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

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
	markerMetadata        byte = 'M' // JSON metadata entries, either direction
	markerServerEndStream byte = 'S' // EndStreamMessage JSON, server -> client
	markerClientEndStream byte = 'C' // end of the request stream, client -> server
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
// type it arrived in, which the caller reads from the transport. An unknown
// marker is the dispatcher's to report; §5.1 reserves nothing else.
func decodeMessage(frame []byte, text bool) (wireMessage, *connect.Error) {
	if len(frame) == 0 {
		return wireMessage{}, errorf(connect.CodeInternal, "protocol error: empty message carries no marker")
	}
	return wireMessage{marker: frame[0], payload: frame[1:], text: text}, nil
}

// textEncodingError reports a text message that is not valid UTF-8, which
// RFC 6455 §8.1 forbids. §6.1 has the receiver check it before reading the
// marker and fail the connection with 1007, without an S message.
//
// Neither this package's WebSocket library nor the layers under it check this,
// so a peer that emits invalid UTF-8 would otherwise have its bytes handed to
// a codec here while a browser on the same wire severs the connection — the
// encoding bug reproducing everywhere except in these tests.
func textEncodingError(frame []byte, text bool) *connect.Error {
	if !text || utf8.Valid(frame) {
		return nil
	}
	return errorf(
		connect.CodeInternal,
		"protocol error: text message is not valid UTF-8",
	)
}

// frameName names a frame type for a diagnostic, which §6.1 requires a
// frame-type error to state.
func frameName(text bool) string {
	if text {
		return "text"
	}
	return "binary"
}

// messageSender writes one encoded message as one WebSocket frame. text says
// which frame type to use.
type messageSender interface {
	send(ctx context.Context, text bool, data []byte) (int64, error)
}

// messageWriter encodes messages and hands them to a sender.
type messageWriter struct {
	sender       messageSender
	sendMaxBytes int
	stats        *connect.MessageStats
}

// writeBody encodes message with the negotiated codec and sends it as a body.
func (w *messageWriter) writeBody(ctx context.Context, codec connect.Codec, message any) *connect.Error {
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	if err := codec.MarshalWrite(ctx, buffer, message); err != nil {
		return errorf(connect.CodeInternal, "marshal message: %w", err)
	}
	// JSON is UTF-8 by construction; a custom text codec is checked, since
	// the peer would otherwise fail the whole connection with 1007 (§6.1).
	bodyIsText := codecIsText(codec)
	if bodyIsText && codec.Name() != connect.CodecNameJSON && !utf8.Valid(buffer.Bytes()) {
		return errorf(
			connect.CodeInternal,
			"marshal message: codec %q produced invalid UTF-8 for a text frame", codec.Name(),
		)
	}
	if err := w.send(ctx, markerBody, bodyIsText, buffer.Bytes()); err != nil {
		return err
	}
	if w.stats != nil {
		*w.stats = connect.MessageStats{Size: buffer.Len()}
	}
	return nil
}

// writeJSON sends a control message, whose payload is always JSON and whose
// frame is therefore always text.
func (w *messageWriter) writeJSON(ctx context.Context, marker byte, value any) *connect.Error {
	data, err := json.Marshal(value)
	if err != nil {
		return errorf(connect.CodeInternal, "marshal %s message: %w", markerName(marker), err)
	}
	return w.send(ctx, marker, true, data)
}

// writeEndStream sends the terminal message. Its EndStreamMessage schema is
// shared with Connect over HTTP; only the framing around it differs.
func (w *messageWriter) writeEndStream(ctx context.Context, err error, trailer http.Header) *connect.Error {
	encoded, trailerErr := encodeTrailer(trailer)
	if trailerErr != nil {
		// As with an unsendable header: the trailers are dropped, and the
		// unsendable one is the outcome unless the handler reported its own.
		encoded = nil
		if err == nil {
			err = trailerErr
		}
	}
	end := &connectwire.EndStreamMessage{Trailer: encoded}
	if err != nil {
		end.Error = connectwire.NewWireError(err)
	}
	return w.writeJSON(ctx, markerServerEndStream, end)
}

// send encodes one message and hands it to the sender.
func (w *messageWriter) send(ctx context.Context, marker byte, text bool, payload []byte) *connect.Error {
	// §12 leaves the marker out of the limit, as Connect over HTTP leaves out
	// its envelope, so one configured size means the same on both transports.
	// Only an RPC message is limited: an M or S over it would leave the stream
	// without its opening or its verdict, which §9 requires.
	if marker == markerBody && w.sendMaxBytes > 0 && len(payload) > w.sendMaxBytes {
		return errorf(
			connect.CodeResourceExhausted,
			"message size %d exceeds sendMaxBytes %d", len(payload), w.sendMaxBytes,
		)
	}
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	encodeMessage(buffer, marker, payload)
	if _, sendErr := w.sender.send(ctx, text, buffer.Bytes()); sendErr != nil {
		wrapped := connecterr.WrapIfContextDone(ctx, sendErr)
		if connectErr, ok := connecterr.AsError(wrapped); ok {
			return connectErr
		}
		return errorf(connect.CodeUnknown, "write message: %w", wrapped)
	}
	return nil
}

// An M message's metadata is a JSON array of entries, each a key followed by
// all of that key's values: [["acme-tenant","42"],["accept","a","b"]]. An
// array is used instead of an object because some JSON parsers automatically
// swallow duplicate keys and won't report if a key repeats. A repeated key is
// a protocol error.
//
// Keys are lower-case on the wire, matching HTTP/2 and HTTP/3. A key ending in
// -bin carries base64, because JSON strings are Unicode and arbitrary bytes
// are not. As over HTTP, the application holds that base64 form:
// connect.EncodeBinaryHeader and connect.DecodeBinaryHeader convert it, and
// this package passes it through.

// metadataBinarySuffix marks a key whose values are base64-encoded bytes.
const metadataBinarySuffix = "-bin"

// encodeMetadata renders headers as M message entries, in key order so the
// wire form is deterministic. Keys must be canonical, so no two collide. A key
// with no values is left out: §7.3.1 gives every entry at least one value.
//
// §7.3.1 binds the sender too, so a header that breaks its rules is refused
// rather than sent for the peer to reject. Values are trimmed of surrounding
// space and tab, which a sender should not send, and -bin values are sent
// unpadded.
func encodeMetadata(header http.Header) ([][]string, *connect.Error) {
	entries := make([][]string, 0, len(header))
	for _, key := range slices.Sorted(maps.Keys(header)) {
		if len(header[key]) == 0 {
			continue
		}
		lower := strings.ToLower(key)
		if !validHeaderFieldName(lower) {
			return nil, errorf(connect.CodeInternal, "metadata key %q is not a valid HTTP field name", key)
		}
		binary := strings.HasSuffix(lower, metadataBinarySuffix)
		entry := make([]string, 0, 1+len(header[key]))
		entry = append(entry, lower)
		for _, value := range header[key] {
			value = strings.Trim(value, " \t")
			if binary {
				value = strings.TrimRight(value, "=")
				if _, err := base64.RawStdEncoding.DecodeString(value); err != nil {
					return nil, errorf(
						connect.CodeInternal, "metadata %q has a value that is not base64: %w", key, err,
					)
				}
			} else if !validHeaderFieldValue(value) {
				return nil, errorf(
					connect.CodeInternal,
					"metadata %q has a value that is not a valid HTTP field value; values are visible "+
						"ASCII, space, and tab, and anything else goes base64-encoded under a -bin key", key,
				)
			}
			entry = append(entry, value)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// checkRequestMetadata refuses a header a client may not send in its M
// message (§7.3.2). The server's deny list is its own, so only the default
// one is known here.
func checkRequestMetadata(header http.Header) *connect.Error {
	for _, key := range slices.Sorted(maps.Keys(header)) {
		if reason, reserved := reservedHeaderReason(key, defaultInfrastructureHeaders()); reserved {
			return errorf(
				connect.CodeInternal,
				"request header %q cannot be sent over WebSocket: %s", key, reason,
			)
		}
	}
	return nil
}

// encodeTrailer renders trailers for an S message, whose EndStreamMessage
// schema is shared with Connect over HTTP and so remains a JSON object. Only a
// server sends one, and §9 keeps connect- keys out of it. Names and values are
// held to the rules an M message's are, so a receiver never sees a trailer it
// would have to reject.
func encodeTrailer(trailer http.Header) (http.Header, *connect.Error) {
	sendable := make(http.Header, len(trailer))
	for key, values := range trailer {
		if !isConnectReserved(strings.ToLower(key)) {
			sendable[key] = values
		}
	}
	entries, err := encodeMetadata(sendable)
	if err != nil {
		return nil, err
	}
	encoded := make(http.Header, len(entries))
	for _, entry := range entries {
		encoded[entry[0]] = entry[1:]
	}
	return encoded, nil
}

// decodeTrailer reads an S message's trailers. They keep Connect-over-HTTP's
// object form and its rules (§9), so they are held to the shared rules for
// names and values but not to the M message's lower-case and non-empty ones.
func decodeTrailer(trailer http.Header) (http.Header, *connect.Error) {
	entries := make([][]string, 0, len(trailer))
	for key, values := range trailer {
		entries = append(entries, append([]string{key}, values...))
	}
	return decodeEntries(entries, false)
}

// decodeMetadata reads an M message's entries into an http.Header, enforcing
// §7.3.1: every key is lower-case and has at least one value, and no key
// appears twice. Keys are canonical MIME form in memory so that Go's own
// accessors work.
func decodeMetadata(entries [][]string) (http.Header, *connect.Error) {
	return decodeEntries(entries, true)
}

// decodeEntries holds entries to the rules every metadata key and value
// shares; strict adds the M message's own. A -bin value is passed through as
// base64 without being decoded: as over HTTP, a bad value surfaces when the
// application decodes it (§7.3.1).
func decodeEntries(entries [][]string, strict bool) (http.Header, *connect.Error) {
	header := make(http.Header, len(entries))
	spelling := make(map[string]string, len(entries))
	for _, entry := range entries {
		if len(entry) == 0 {
			return nil, errorf(
				connect.CodeInternal,
				"metadata entry is empty; an entry is a key followed by its values",
			)
		}
		key, values := entry[0], entry[1:]
		if strict && len(values) == 0 {
			return nil, errorf(
				connect.CodeInternal,
				"metadata %q has no values; every key carries at least one, which may be empty", key,
			)
		}
		canonical := http.CanonicalHeaderKey(key)
		if first, duplicate := spelling[canonical]; duplicate {
			return nil, duplicateMetadataKeyError(first, key)
		}
		spelling[canonical] = key
		if !validHeaderFieldName(key) {
			return nil, errorf(
				connect.CodeInternal,
				"metadata key %q is not a valid HTTP field name", key,
			)
		}
		if strict && key != strings.ToLower(key) {
			return nil, errorf(
				connect.CodeInternal,
				"metadata key %q is not lower-case; keys are lower-case on the wire", key,
			)
		}
		// §7.3.1: every value is trimmed before delivery, -bin ones included;
		// only the character rules are for text values alone.
		binary := strings.HasSuffix(strings.ToLower(key), metadataBinarySuffix)
		trimmed := make([]string, len(values))
		for index, value := range values {
			if !binary && !validHeaderFieldValue(value) {
				return nil, errorf(
					connect.CodeInternal,
					"metadata %q has a value that is not a valid HTTP field value; "+
						"values are visible ASCII, space, and tab", key,
				)
			}
			trimmed[index] = strings.Trim(value, " \t")
		}
		header[canonical] = trimmed
	}
	return header, nil
}

// validHeaderFieldName reports whether key is an RFC 9110 token. Metadata
// stands in for HTTP headers and may be forwarded as one, so a key that is not
// a field name has no way to survive the journey.
func validHeaderFieldName(key string) bool {
	if key == "" {
		return false
	}
	for index := range len(key) {
		if !isTokenByte(key[index]) {
			return false
		}
	}
	return true
}

func isTokenByte(char byte) bool {
	switch {
	case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", char) >= 0
}

// validHeaderFieldValue reports whether value holds only visible ASCII, space,
// and tab (§7.3.1). RFC 9110's obs-text is excluded because a JSON string has
// no byte form for it, and control bytes would end or split a forwarded header.
func validHeaderFieldValue(value string) bool {
	for index := range len(value) {
		char := value[index]
		if char != '\t' && (char < ' ' || char > '~') {
			return false
		}
	}
	return true
}

// duplicateMetadataKeyError names both spellings in a stable order, so the
// message does not depend on which one was reached first. Trailers arrive as
// a map, so for them that order is random.
func duplicateMetadataKeyError(one, other string) *connect.Error {
	if one > other {
		one, other = other, one
	}
	return errorf(
		connect.CodeInternal,
		"metadata keys %q and %q are the same key; a key carries all its values in one entry",
		one, other,
	)
}
