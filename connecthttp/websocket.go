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

	websocketStatusClientError websocket.StatusCode = 3102

	websocketCloseGrace = time.Second
)

var (
	// After a protocol error a server closes with 1002. A client closes with
	// 3102.
	errWebSocketProtocol = errors.New("protocol error")
	// Both sides close with 1007. A server sends no S.
	errWebSocketInvalidUTF8 = connect.NewError(connect.CodeInternal, "invalid UTF-8 in text message")
	errWebSocketHTTPClient  = errors.New("HTTP client can't carry a WebSocket")
	errWebSocketFinished    = connect.NewError(connect.CodeCanceled, "stream finished")

	// httpTokenBytes follows RFC 9110 Section 5.6.2.
	//
	//nolint:gochecknoglobals
	httpTokenBytes = func() (table [256]bool) {
		for char := range table {
			table[char] = ('a' <= char && char <= 'z') ||
				('A' <= char && char <= 'Z') ||
				('0' <= char && char <= '9') ||
				strings.IndexByte("!#$%&'*+-.^_`|~", byte(char)) >= 0
		}
		return table
	}()
	// websocketValueBytes follows RFC 9110 Section 5.5 without obs-text. A JSON
	// string can't carry the bytes 0x80-0xFF.
	//
	//nolint:gochecknoglobals
	websocketValueBytes = func() (table [256]bool) {
		for char := range table {
			table[char] = char == '\t' || (' ' <= char && char <= '~')
		}
		return table
	}()
)

// WithWebSocket enables Connect over WebSocket.
//
// With [Mount], procedures accept WebSocket upgrades as well as the HTTP
// protocols. With [NewTransport], clients call over WebSocket instead of the
// HTTP protocols. Use [WithConditionalOptions] to enable it for some
// procedures.
//
// Request metadata travels in the first WebSocket message. A metadata key
// that is also a handshake header fails the RPC. HTTP middleware sees only
// the handshake. Authenticate RPCs in a [connect.ServerInterceptor] or in the
// procedure.
//
// The handshake uses HTTP/1.1. An HTTP/2-only client can't dial.
//
// An [*http.Client] works as is. Its Timeout bounds only the handshake. Use
// a context deadline to bound an RPC.
//
// Any other [HTTPClient] must return the 101 response with its body
// unchanged. The body must implement [io.ReadWriteCloser]. A client that
// buffers or wraps the body fails the dial with [connect.CodeInternal]. An
// inner [*http.Client] with a Timeout wraps the body. The client follows
// redirects with its own policy. It can't follow a redirect to a ws or wss
// URL.
func WithWebSocket() Option {
	return websocketOption{}
}

type websocketOption struct{}

func (websocketOption) apply(opts *options) {
	opts.protocol = protocolNameWebSocket
	opts.websocket = true
}

// websocketReadMessage reads a whole message into dst. It reuses dst where
// conn.Read allocates a new slice per message.
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

// websocketParseMarker removes the marker byte from buffer and returns it.
// The payload stays in buffer.
func websocketParseMarker(messageType websocket.MessageType, buffer *bytes.Buffer, codecName string) (byte, *connect.Error) {
	data := buffer.Bytes()
	text := messageType == websocket.MessageText
	if text && !utf8.Valid(data) {
		return 0, errWebSocketInvalidUTF8
	}
	if len(data) == 0 {
		return 0, newWebSocketProtocolError("empty message")
	}
	marker, payload := data[0], data[1:]
	switch marker {
	case websocketMarkerBody:
		if bodyType := websocketBodyType(codecName); messageType != bodyType {
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
	buffer.Next(1)
	return marker, nil
}

// websocketTrimMetadataValue removes what RFC 9110 Section 5.5 says is not
// part of a field value.
func websocketTrimMetadataValue(value string) string {
	return strings.Trim(value, " \t")
}

func websocketMarshalMetadata(buffer *bytes.Buffer, metadata http.Header) {
	buffer.WriteByte(websocketMarkerLeadingMetadata)
	entries := make([][]string, 0, len(metadata))
	for key, values := range metadata {
		entries = append(entries, append([]string{key}, values...))
	}
	slices.SortFunc(entries, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
	_ = websocketAppendJSON(buffer, entries)
}

func websocketAppendJSON(buffer *bytes.Buffer, value any) error {
	if err := json.NewEncoder(buffer).Encode(value); err != nil {
		return err
	}
	// Encode ends with a newline.
	buffer.Truncate(buffer.Len() - 1)
	return nil
}

func websocketDecodeMetadata(payload []byte, fromClient bool) (http.Header, *connect.Error) {
	// encoding/json decodes a JSON null into a string as "".
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

func websocketValidateMetadata(key string, values []string, fromClient bool) *connect.Error {
	switch {
	case key == "":
		return newWebSocketProtocolError("M message has an empty key")
	case !websocketIsMetadataKey(key):
		return newWebSocketProtocolError("M message key %q is not a valid header name", key)
	case strings.ToLower(key) != key:
		return newWebSocketProtocolError("M message key %q is not lower-case", key)
	}
	if fromClient && websocketIsReservedKey(key) {
		return newWebSocketProtocolError("M message key %q is reserved", key)
	}
	// RFC 008 §7.3.1 leaves invalid base64 to the application.
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

func websocketValidateTrailer(key string, values []string) *connect.Error {
	if !websocketIsMetadataKey(key) {
		return newWebSocketProtocolError("S message key %q is not a valid header name", key)
	}
	if strings.HasSuffix(strings.ToLower(key), "-bin") {
		return nil
	}
	for _, value := range values {
		if !websocketIsMetadataValue(websocketTrimMetadataValue(value)) {
			return newWebSocketProtocolError("S message value for key %q is not a valid header value", key)
		}
	}
	return nil
}

// websocketIsReservedKey follows the three lists in spec Section 7.3.2.
func websocketIsReservedKey(key string) bool {
	switch key {
	case
		// 1. Fetch forbidden request headers.
		"accept-charset", "accept-encoding", "access-control-request-headers",
		"access-control-request-method", "connection", "content-length", "cookie",
		"cookie2", "date", "dnt", "expect", "host", "keep-alive", "origin",
		"referer", "set-cookie", "te", "trailer", "transfer-encoding", "upgrade",
		"via", "x-http-method", "x-http-method-override", "x-method-override",
		// 2. Infrastructure deny list.
		"forwarded", "x-real-ip",
		// 3. Controlled by this protocol.
		"content-type", "content-encoding":
		return true
	}
	for _, prefix := range []string{
		"proxy-", "sec-", // 1. Fetch forbidden request headers.
		"x-forwarded-", // 2. Infrastructure deny list.
		"connect-",     // 3. Controlled by this protocol.
	} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func websocketIsMetadataKey(key string) bool {
	if key == "" {
		return false
	}
	for i := range len(key) {
		if !httpTokenBytes[key[i]] {
			return false
		}
	}
	return true
}

func websocketIsMetadataValue(value string) bool {
	for i := range len(value) {
		if !websocketValueBytes[value[i]] {
			return false
		}
	}
	return true
}

// websocketReadLimit returns the message read limit for coder. readMaxBytes
// caps the payload, and the message limit is one more for the marker byte.
// readMaxBytes of zero or less means no limit, which coder spells -1.
func websocketReadLimit(readMaxBytes int) int64 {
	if readMaxBytes <= 0 {
		return -1
	}
	return int64(readMaxBytes) + 1
}

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

func websocketBodyType(codecName string) websocket.MessageType {
	if codecName == connect.CodecNameJSON {
		return websocket.MessageText
	}
	return websocket.MessageBinary
}

// websocketGraceContext ends [websocketCloseGrace] after ctx. coder drops the
// connection when a read or write context ends, so a nearly done write or
// close handshake gets time to finish. coder bounds the close handshake at 5s
// but not the drain of a partly read frame.
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

func websocketCause(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// newWebSocketDecodeError uses invalid_argument, as RFC 008 §6.1 requires.
func newWebSocketDecodeError(err error) *connect.Error {
	return connect.NewError(connect.CodeInvalidArgument, "protocol error: unmarshal message: "+err.Error()).WithCause(errWebSocketProtocol)
}

func websocketFrameName(messageType websocket.MessageType) string {
	if messageType == websocket.MessageText {
		return "text"
	}
	return "binary"
}
