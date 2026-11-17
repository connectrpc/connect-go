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

package connecthttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/bufferpool"
	"github.com/coder/websocket"
)

// Connect over WebSocket carries one RPC per WebSocket connection. Every
// message starts with a one byte marker that names its type. The codec is
// picked by the WebSocket subprotocol. See the Connect over WebSocket RFC for
// the full protocol.

const (
	// protocolNameWebSocket is reported as [connect.CallInfo].Protocol and
	// selects the WebSocket client.
	protocolNameWebSocket = "connect-websocket"

	websocketSubprotocolPrefix     = "connectrpc.1+"
	websocketTimeoutQueryParameter = "connect-timeout-ms"

	websocketMarkerBody            = 'B'
	websocketMarkerLeadingMetadata = 'M'
	websocketMarkerServerEndStream = 'S'
	websocketMarkerClientEndStream = 'C'

	// websocketStatusClientError is the close code a client sends when it
	// ends the connection because of its own error or a peer's protocol error.
	websocketStatusClientError websocket.StatusCode = 3102
)

var (
	// errWebSocketProtocol is the cause of every protocol error. A server
	// closes with 1002 after one and a client closes with 3102.
	errWebSocketProtocol = errors.New("protocol error")
	// errWebSocketInvalidUTF8 is the cause when a text message is not valid
	// UTF-8. Both sides close with 1007, and a server sends no end of stream.
	errWebSocketInvalidUTF8 = errors.New("invalid UTF-8 in text message")
)

// WithWebSocket enables Connect over WebSocket.
//
// On [Mount] each procedure accepts WebSocket upgrades as well as the HTTP
// protocols. On [NewTransport] clients use WebSocket in place of the Connect
// protocol. Use [WithConditionalOptions] to enable it for some procedures
// only.
//
// Request metadata travels in the first WebSocket message and replaces
// handshake headers with the same name. Authenticate RPCs in an interceptor
// or handler, not in HTTP middleware that only sees the handshake.
func WithWebSocket() Option {
	return websocketOption{}
}

type websocketOption struct{}

func (websocketOption) apply(opts *options) {
	opts.protocol = protocolNameWebSocket
	opts.websocket = true
}

// websocketMessage is one Connect over WebSocket message with its marker split
// from its payload.
type websocketMessage struct {
	marker  byte
	payload []byte
	text    bool
}

// parseWebSocketMessage checks a message as read from the connection and
// splits off its marker. bodyIsText is true when the negotiated codec sends
// bodies as text. Ordering and direction are left to the caller.
func parseWebSocketMessage(messageType websocket.MessageType, data []byte, bodyIsText bool) (websocketMessage, *connect.Error) {
	text := messageType == websocket.MessageText
	if text && !utf8.Valid(data) {
		return websocketMessage{}, connect.NewError(connect.CodeInternal, errWebSocketInvalidUTF8.Error()).WithCause(errWebSocketInvalidUTF8)
	}
	if len(data) == 0 {
		return websocketMessage{}, newWebSocketProtocolError("empty message")
	}
	message := websocketMessage{marker: data[0], payload: data[1:], text: text}
	switch message.marker {
	case websocketMarkerBody:
		if text != bodyIsText {
			return websocketMessage{}, newWebSocketProtocolError("B message in a %s frame, expected a %s frame", websocketFrameName(text), websocketFrameName(bodyIsText))
		}
		// Only JSON is sent as text, and an empty JSON message is "{}".
		if text && len(message.payload) == 0 {
			return websocketMessage{}, newWebSocketProtocolError("B message has no payload")
		}
	case websocketMarkerLeadingMetadata, websocketMarkerServerEndStream, websocketMarkerClientEndStream:
		if !text {
			return websocketMessage{}, newWebSocketProtocolError("%c message in a binary frame, expected a text frame", message.marker)
		}
		if message.marker == websocketMarkerClientEndStream && len(message.payload) > 0 {
			return websocketMessage{}, newWebSocketProtocolError("C message has a payload")
		}
		if message.marker != websocketMarkerClientEndStream && len(message.payload) == 0 {
			return websocketMessage{}, newWebSocketProtocolError("%c message has no payload", message.marker)
		}
	default:
		return websocketMessage{}, newWebSocketProtocolError("unknown marker 0x%02x", message.marker)
	}
	return message, nil
}

// encodeWebSocketMetadata returns the payload of an M message for header. Keys
// are lower-cased and sorted. Keys without values and keys starting with
// "connect-" are skipped.
func encodeWebSocketMetadata(header http.Header) []byte {
	merged := make(map[string][]string, len(header))
	for key, values := range header {
		key = strings.ToLower(key)
		if len(values) == 0 || strings.HasPrefix(key, "connect-") {
			continue
		}
		merged[key] = append(merged[key], values...)
	}
	entries := make([][]string, 0, len(merged))
	for key, values := range merged {
		entries = append(entries, append([]string{key}, values...))
	}
	slices.SortFunc(entries, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
	payload, err := json.Marshal(entries)
	if err != nil {
		// Marshaling a slice of string slices cannot fail.
		return []byte("[]")
	}
	return payload
}

// decodeWebSocketMetadata parses and validates the payload of an M message.
// fromClient also rejects keys a client must not send. Keys are canonicalized
// so they replace handshake headers of the same name.
func decodeWebSocketMetadata(payload []byte, fromClient bool) (http.Header, *connect.Error) {
	// Pointers tell a JSON null apart from an empty string.
	var entries [][]*string
	if err := json.Unmarshal(payload, &entries); err != nil {
		return nil, newWebSocketProtocolError("M message is not an array of string arrays: %v", err)
	}
	if entries == nil {
		return nil, newWebSocketProtocolError("M message is null, expected an array")
	}
	header := make(http.Header, len(entries))
	for _, entry := range entries {
		if len(entry) < 2 {
			return nil, newWebSocketProtocolError("M message entry needs a key and at least one value")
		}
		if slices.Contains(entry, nil) {
			return nil, newWebSocketProtocolError("M message entry has a null element")
		}
		key := *entry[0]
		values := make([]string, len(entry)-1)
		for i, value := range entry[1:] {
			values[i] = *value
		}
		if err := validateWebSocketMetadata(key, values, fromClient); err != nil {
			return nil, err
		}
		canonical := http.CanonicalHeaderKey(key)
		if _, ok := header[canonical]; ok {
			return nil, newWebSocketProtocolError("M message has key %q more than once", key)
		}
		header[canonical] = values
	}
	return header, nil
}

// validateWebSocketMetadata checks one M message entry. A client checks its
// own metadata with it before sending.
func validateWebSocketMetadata(key string, values []string, fromClient bool) *connect.Error {
	if key == "" {
		return newWebSocketProtocolError("M message has an empty key")
	}
	for i := range len(key) {
		char := key[i]
		if char >= 'A' && char <= 'Z' {
			return newWebSocketProtocolError("M message key %q is not lower-case", key)
		}
		if !isHTTPTokenByte(char) {
			return newWebSocketProtocolError("M message key %q is not a valid header name", key)
		}
	}
	if fromClient && isReservedWebSocketKey(key) {
		return newWebSocketProtocolError("M message key %q is reserved", key)
	}
	// Values of -bin keys are base64. Invalid base64 is reported when the
	// application decodes the value, not here.
	if strings.HasSuffix(key, "-bin") {
		return nil
	}
	for _, value := range values {
		if !isHTTPFieldValue(value) {
			return newWebSocketProtocolError("M message value for key %q is not a valid header value", key)
		}
	}
	return nil
}

// isReservedWebSocketKey reports whether a client must not send key, which is
// lower-case. The list is the Fetch standard's forbidden request headers, the
// headers this protocol controls, and headers that proxies set.
func isReservedWebSocketKey(key string) bool {
	switch key {
	case "accept-charset", "accept-encoding", "access-control-request-headers",
		"access-control-request-method", "connection", "content-length", "cookie",
		"cookie2", "date", "dnt", "expect", "host", "keep-alive", "origin",
		"referer", "set-cookie", "te", "trailer", "transfer-encoding", "upgrade",
		"via", "x-http-method", "x-http-method-override", "x-method-override",
		"content-type", "content-encoding",
		"forwarded", "x-real-ip":
		return true
	}
	for _, prefix := range []string{"proxy-", "sec-", "connect-", "x-forwarded-"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// isHTTPTokenByte reports whether char may appear in an HTTP token, such as a
// header name (RFC 9110 Section 5.6.2).
func isHTTPTokenByte(char byte) bool {
	switch {
	case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		return true
	default:
		return strings.IndexByte("!#$%&'*+-.^_`|~", char) >= 0
	}
}

// isHTTPFieldValue reports whether value is a valid HTTP field value. Control
// characters other than horizontal tab are not allowed (RFC 9110 Section 5.5).
func isHTTPFieldValue(value string) bool {
	for i := range len(value) {
		if char := value[i]; (char < ' ' && char != '\t') || char == 0x7f {
			return false
		}
	}
	return true
}

// websocketSubprotocol returns the subprotocol token for a codec.
func websocketSubprotocol(codecName string) string {
	return websocketSubprotocolPrefix + codecName
}

// websocketBodyIsText reports whether B messages for a codec are sent as text.
// JSON is text and every other codec is binary.
func websocketBodyIsText(codecName string) bool {
	return codecName == connect.CodecNameJSON
}

// websocketReadLimit returns the read limit for a connection. It allows one
// byte for the marker. Zero readMaxBytes means no limit.
func websocketReadLimit(readMaxBytes int) int64 {
	if readMaxBytes <= 0 {
		return -1
	}
	return int64(readMaxBytes) + 1
}

// writeWebSocketBody marshals msg and writes it as a B message.
func writeWebSocketBody(ctx context.Context, conn *websocket.Conn, codec connect.Codec, msg any) error {
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	buffer.WriteByte(websocketMarkerBody)
	if err := codec.MarshalWrite(ctx, buffer, msg); err != nil {
		return connect.Errorf(connect.CodeInternal, "marshal message: %s", err).WithCause(err)
	}
	messageType := websocket.MessageBinary
	if websocketBodyIsText(codec.Name()) {
		messageType = websocket.MessageText
	}
	return conn.Write(ctx, messageType, buffer.Bytes())
}

// writeWebSocketText writes marker and payload as one text message.
func writeWebSocketText(ctx context.Context, conn *websocket.Conn, marker byte, payload []byte) error {
	message := make([]byte, 0, len(payload)+1)
	message = append(message, marker)
	message = append(message, payload...)
	return conn.Write(ctx, websocket.MessageText, message)
}

func newWebSocketProtocolError(format string, args ...any) *connect.Error {
	message := fmt.Sprintf(format, args...)
	return connect.NewError(connect.CodeInternal, "protocol error: "+message).WithCause(errWebSocketProtocol)
}

func websocketFrameName(text bool) string {
	if text {
		return "text"
	}
	return "binary"
}
