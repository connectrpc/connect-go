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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/bufferpool"
	"github.com/coder/websocket"
)

const (
	protocolNameWebSocket = "connect-websocket"

	websocketSubprotocolPrefix     = "connectrpc.1+"
	websocketTimeoutQueryParameter = "connect-timeout-ms"

	websocketMarkerBody            = 'B'
	websocketMarkerLeadingMetadata = 'M'
	websocketMarkerServerEndStream = 'S'
	websocketMarkerClientEndStream = 'C'

	// The close code a client sends for its own or a peer's protocol error.
	websocketStatusClientError websocket.StatusCode = 3102

	websocketCloseGrace = time.Second
)

var (
	// A server closes with 1002 after a protocol error and a client with 3102.
	errWebSocketProtocol = errors.New("protocol error")
	// Both sides close with 1007, and a server sends no S.
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
//
// Clients need an [*http.Client] as their [HTTPClient]. Its Timeout covers
// only the handshake, so use a context deadline to bound an RPC. The
// handshake is HTTP/1.1, so a client that only speaks HTTP/2 can't dial. A
// server that answers with a bad upgrade can delay the dial error by up to
// three seconds.
func WithWebSocket() Option {
	return websocketOption{}
}

type websocketOption struct{}

func (websocketOption) apply(opts *options) {
	opts.protocol = protocolNameWebSocket
	opts.websocket = true
}

type websocketMessage struct {
	marker  byte
	payload []byte
}

// parseWebSocketMessage leaves ordering and direction checks to the caller.
func parseWebSocketMessage(messageType websocket.MessageType, data []byte, bodyIsText bool) (websocketMessage, *connect.Error) {
	text := messageType == websocket.MessageText
	if text && !utf8.Valid(data) {
		return websocketMessage{}, connect.NewError(connect.CodeInternal, errWebSocketInvalidUTF8.Error()).WithCause(errWebSocketInvalidUTF8)
	}
	if len(data) == 0 {
		return websocketMessage{}, newWebSocketProtocolError("empty message")
	}
	message := websocketMessage{marker: data[0], payload: data[1:]}
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

func websocketMetadata(header *connect.Header) http.Header {
	metadata := make(http.Header)
	for key, values := range userHeaders(header) {
		key = strings.ToLower(key)
		if len(values) == 0 || strings.HasPrefix(key, "connect-") {
			continue
		}
		metadata[key] = append(metadata[key], values...)
	}
	return metadata
}

func encodeWebSocketMetadata(metadata http.Header) []byte {
	entries := make([][]string, 0, len(metadata))
	for key, values := range metadata {
		entries = append(entries, append([]string{key}, values...))
	}
	slices.SortFunc(entries, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
	payload, _ := json.Marshal(entries) //nolint:errchkjson // a slice of string slices always marshals
	return payload
}

// decodeWebSocketMetadata canonicalizes keys so they replace handshake headers
// of the same name.
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

// dropInvalidWebSocketMetadata drops entries a peer would reject, as net/http
// drops invalid header fields.
func dropInvalidWebSocketMetadata(metadata http.Header) http.Header {
	for key, values := range metadata {
		if validateWebSocketMetadata(key, nil, false) != nil {
			delete(metadata, key)
			continue
		}
		if !strings.HasSuffix(key, "-bin") {
			values = slices.DeleteFunc(values, func(value string) bool { return !isHTTPFieldValue(value) })
		}
		if len(values) == 0 {
			delete(metadata, key)
		} else {
			metadata[key] = values
		}
	}
	return metadata
}

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

// isReservedWebSocketKey lists the Fetch standard's forbidden request headers,
// headers this protocol controls and headers proxies set.
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

// isHTTPTokenByte follows RFC 9110 Section 5.6.2.
func isHTTPTokenByte(char byte) bool {
	switch {
	case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		return true
	default:
		return strings.IndexByte("!#$%&'*+-.^_`|~", char) >= 0
	}
}

// isHTTPFieldValue follows RFC 9110 Section 5.5.
func isHTTPFieldValue(value string) bool {
	for i := range len(value) {
		if char := value[i]; (char < ' ' && char != '\t') || char == 0x7f {
			return false
		}
	}
	return true
}

func websocketBodyIsText(codecName string) bool {
	return codecName == connect.CodecNameJSON
}

// websocketReadLimit doesn't count the marker byte. -1 lifts coder's 32 KiB
// default.
func websocketReadLimit(readMaxBytes int) int64 {
	if readMaxBytes <= 0 {
		return -1
	}
	return int64(readMaxBytes) + 1
}

// marshalWebSocketBody returns a pooled buffer. The caller must
// bufferpool.Put it.
func marshalWebSocketBody(ctx context.Context, codec connect.Codec, msg any, sendMaxBytes int) (*bytes.Buffer, error) {
	buffer := bufferpool.Get()
	buffer.WriteByte(websocketMarkerBody)
	if err := codec.MarshalWrite(ctx, buffer, msg); err != nil {
		bufferpool.Put(buffer)
		return nil, connect.Errorf(connect.CodeInternal, "marshal message: %s", err).WithCause(err)
	}
	if size := buffer.Len() - 1; sendMaxBytes > 0 && size > sendMaxBytes {
		bufferpool.Put(buffer)
		return nil, connect.Errorf(connect.CodeResourceExhausted, "message size %d exceeds sendMaxBytes %d", size, sendMaxBytes)
	}
	return buffer, nil
}

func websocketBodyType(codecName string) websocket.MessageType {
	if websocketBodyIsText(codecName) {
		return websocket.MessageText
	}
	return websocket.MessageBinary
}

func writeWebSocketText(ctx context.Context, conn *websocket.Conn, marker byte, payload []byte) error {
	message := make([]byte, 0, len(payload)+1)
	message = append(message, marker)
	message = append(message, payload...)
	return conn.Write(ctx, websocket.MessageText, message)
}

// websocketGraceContext ends [websocketCloseGrace] after ctx. coder drops the
// connection when a read or write context ends, so a nearly done write or
// close handshake gets time to finish. coder bounds Close at 5s on its own.
func websocketGraceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	graceCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() {
		time.AfterFunc(websocketCloseGrace, cancel)
	})
	return graceCtx, func() {
		stop()
		cancel()
	}
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
