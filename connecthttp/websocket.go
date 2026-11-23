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
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
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
	// An HTTPClient with a Timeout, or one that buffers or wraps the
	// response body, can't carry a WebSocket.
	errWebSocketHTTPClient = errors.New("HTTP client can't carry a WebSocket")
)

// WithWebSocket enables Connect over WebSocket.
//
// On [Mount], procedures accept WebSocket upgrades alongside the HTTP
// protocols. On [NewTransport], clients use WebSocket instead of the HTTP
// protocols. Use [WithConditionalOptions] to enable it for specific
// procedures only.
//
// Request metadata travels in the first WebSocket message. Sending a
// metadata key that is also a handshake header fails the RPC. Authenticate
// RPCs in a [connect.ServerInterceptor] or procedure. HTTP middleware only
// sees the initial handshake.
//
// An [*http.Client] works as is, and its Timeout only bounds the handshake.
// Use a context deadline to bound an RPC. Any other [HTTPClient] makes the
// handshake request and must return the 101 response unchanged. Its
// response body must be writable and implement [io.ReadWriteCloser]. Any
// other [HTTPClient] with a Timeout fails the dial with
// [connect.CodeInternal]. Buffering or wrapping the response body also fails
// with [connect.CodeInternal].
//
// The handshake uses HTTP/1.1, so an HTTP/2-only client cannot dial. The
// [HTTPClient] follows redirects with its own policy, so it cannot follow
// redirects to ws or wss URLs.
func WithWebSocket() Option {
	return websocketOption{}
}

type websocketOption struct{}

func (websocketOption) apply(opts *options) {
	opts.protocol = protocolNameWebSocket
	opts.websocket = true
}

// websocketReadMessage reads a whole message into dst, which can be recycled.
// conn.Read allocates each message.
func websocketReadMessage(ctx context.Context, conn *websocket.Conn, dst *bytes.Buffer) (websocket.MessageType, error) {
	messageType, reader, err := conn.Reader(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := dst.ReadFrom(reader); err != nil {
		return 0, err
	}
	return messageType, nil
}

// websocketParseMarker returns the marker of a message, leaving ordering and
// direction checks to the caller.
func websocketParseMarker(messageType websocket.MessageType, data []byte, bodyType websocket.MessageType) (byte, *connect.Error) {
	text := messageType == websocket.MessageText
	if text && !utf8.Valid(data) {
		return 0, connect.NewError(connect.CodeInternal, errWebSocketInvalidUTF8.Error()).WithCause(errWebSocketInvalidUTF8)
	}
	if len(data) == 0 {
		return 0, newWebSocketProtocolError("empty message")
	}
	marker, payload := data[0], data[1:]
	switch marker {
	case websocketMarkerBody:
		if messageType != bodyType {
			return 0, newWebSocketProtocolError("B message in a %s frame, expected a %s frame", websocketFrameName(messageType), websocketFrameName(bodyType))
		}
	case websocketMarkerLeadingMetadata, websocketMarkerServerEndStream, websocketMarkerClientEndStream:
		if !text {
			return 0, newWebSocketProtocolError("%c message in a binary frame, expected a text frame", marker)
		}
		if marker == websocketMarkerClientEndStream && len(payload) > 0 {
			return 0, newWebSocketProtocolError("C message has a payload")
		}
		if marker != websocketMarkerClientEndStream && len(payload) == 0 {
			return 0, newWebSocketProtocolError("%c message has no payload", marker)
		}
	default:
		return 0, newWebSocketProtocolError("unknown marker 0x%02x", marker)
	}
	return marker, nil
}

func websocketMetadata(header *connect.Header) http.Header {
	metadata := make(http.Header)
	for key, values := range userHeaders(header) {
		key = strings.ToLower(key)
		if len(values) == 0 || strings.HasPrefix(key, "connect-") {
			continue
		}
		for _, value := range values {
			metadata[key] = append(metadata[key], websocketTrimMetadataValue(value))
		}
	}
	return metadata
}

// websocketTrimMetadataValue removes what RFC 9110 Section 5.5 says is not
// part of a field value.
func websocketTrimMetadataValue(value string) string {
	return strings.Trim(value, " \t")
}

func websocketEncodeMetadata(buffer *bytes.Buffer, metadata http.Header) {
	entries := make([][]string, 0, len(metadata))
	for key, values := range metadata {
		entries = append(entries, append([]string{key}, values...))
	}
	slices.SortFunc(entries, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
	_ = websocketAppendJSON(buffer, entries) // a slice of string slices always encodes
}

// websocketAppendJSON encodes value into buffer, where json.Marshal would
// allocate.
func websocketAppendJSON(buffer *bytes.Buffer, value any) error {
	if err := json.NewEncoder(buffer).Encode(value); err != nil {
		return err
	}
	// Encode ends with a newline.
	buffer.Truncate(buffer.Len() - 1)
	return nil
}

// websocketDecodeMetadata canonicalizes keys so they can be compared with
// handshake headers.
func websocketDecodeMetadata(payload []byte, fromClient bool) (http.Header, *connect.Error) {
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
			values[i] = websocketTrimMetadataValue(*value)
		}
		if err := websocketValidateMetadata(key, values, fromClient); err != nil {
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

// websocketDropInvalidMetadata drops entries a peer would reject, as net/http
// drops invalid header fields.
func websocketDropInvalidMetadata(metadata http.Header) http.Header {
	for key, values := range metadata {
		if websocketValidateMetadata(key, nil, false) != nil {
			delete(metadata, key)
			continue
		}
		if !strings.HasSuffix(key, "-bin") {
			values = slices.DeleteFunc(values, func(value string) bool { return !websocketIsMetadataValue(value) })
		}
		if len(values) == 0 {
			delete(metadata, key)
		} else {
			metadata[key] = values
		}
	}
	return metadata
}

func websocketValidateMetadata(key string, values []string, fromClient bool) *connect.Error {
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
	if fromClient && websocketIsReservedKey(key) {
		return newWebSocketProtocolError("M message key %q is reserved", key)
	}
	// Values of -bin keys are base64. Invalid base64 is reported when the
	// application decodes the value, not here.
	if strings.HasSuffix(key, "-bin") {
		return nil
	}
	for _, value := range values {
		if !websocketIsMetadataValue(value) {
			return newWebSocketProtocolError("M message value for key %q is not a valid header value", key)
		}
	}
	return nil
}

// websocketIsReservedKey lists the Fetch standard's forbidden request headers,
// headers this protocol controls and headers proxies set.
func websocketIsReservedKey(key string) bool {
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
	return (char >= 'a' && char <= 'z') ||
		(char >= 'A' && char <= 'Z') ||
		(char >= '0' && char <= '9') ||
		(strings.IndexByte("!#$%&'*+-.^_`|~", char) >= 0)
}

// websocketIsMetadataValue follows RFC 9110 Section 5.5 without obs-text, the
// bytes 0x80-0xFF, which a JSON string can't carry as bytes.
func websocketIsMetadataValue(value string) bool {
	for i := range len(value) {
		if char := value[i]; (char < ' ' && char != '\t') || char > '~' {
			return false
		}
	}
	return true
}

// websocketReadLimit doesn't count the marker byte. -1 lifts coder's 32 KiB
// default.
func websocketReadLimit(readMaxBytes int) int64 {
	if readMaxBytes <= 0 {
		return -1
	}
	return int64(readMaxBytes) + 1
}

// websocketMarshalBody writes a B message into dst.
func websocketMarshalBody(ctx context.Context, dst *bytes.Buffer, codec connect.Codec, msg any, sendMaxBytes int) error {
	dst.WriteByte(websocketMarkerBody)
	if err := codec.MarshalWrite(ctx, dst, msg); err != nil {
		return connect.Errorf(connect.CodeInternal, "marshal message: %s", err).WithCause(err)
	}
	if size := dst.Len() - 1; sendMaxBytes > 0 && size > sendMaxBytes {
		return connect.Errorf(connect.CodeResourceExhausted, "message size %d exceeds sendMaxBytes %d", size, sendMaxBytes)
	}
	return nil
}

// websocketBodyType sends only JSON as text.
func websocketBodyType(codecName string) websocket.MessageType {
	if codecName == connect.CodecNameJSON {
		return websocket.MessageText
	}
	return websocket.MessageBinary
}

// websocketCloseReason truncates reason to fit a Close frame. coder sends no
// Close frame for a reason over 123 bytes.
func websocketCloseReason(reason string) string {
	const maxReasonBytes = 123
	if len(reason) <= maxReasonBytes {
		return reason
	}
	return strings.ToValidUTF8(reason[:maxReasonBytes], "")
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

// websocketCause stops a cause that wraps io.EOF from reading as a clean end
// of stream.
func websocketCause(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// newWebSocketDecodeError is a protocol error, but it uses invalid_argument to
// match Connect-over-HTTP.
func newWebSocketDecodeError(err error) *connect.Error {
	return connect.NewError(connect.CodeInvalidArgument, "protocol error: unmarshal message: "+err.Error()).WithCause(errWebSocketProtocol)
}

// websocketValidateResponseMetadata rejects a connect- key, which a server
// must not send in M or S.
func websocketValidateResponseMetadata(header *connect.Header, kind string) error {
	for key, values := range header.All() {
		if len(values) > 0 && strings.HasPrefix(key, "Connect-") {
			return connect.Errorf(connect.CodeInternal, "response %s %q is reserved for the protocol", kind, strings.ToLower(key))
		}
	}
	return nil
}

func websocketFrameName(messageType websocket.MessageType) string {
	if messageType == websocket.MessageText {
		return "text"
	}
	return "binary"
}
