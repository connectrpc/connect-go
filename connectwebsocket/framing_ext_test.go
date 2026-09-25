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

package connectwebsocket_test

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

const pingUnaryProcedure = "/connect.ping.v1.PingService/Ping"

// WebSocket opcodes, for tests that build frames by hand.
const (
	opcodeContinuation = 0x00
	opcodeText         = 0x01
	opcodeBinary       = 0x02
)

// readServerFrame reads one frame from the server and reports whether RSV1 is
// set — the bit that says the payload is permessage-deflate compressed. It is
// the only way to observe compression from outside: the library inflates
// transparently, so a decoded message looks the same either way.
func readServerFrame(tb testing.TB, conn net.Conn) (compressed bool, payload []byte) {
	tb.Helper()
	header := make([]byte, 2)
	_, err := io.ReadFull(conn, header)
	assert.Nil(tb, err)
	compressed = header[0]&0x40 != 0 // RSV1
	// Server frames are never masked.
	assert.Equal(tb, header[1]&0x80, byte(0))

	size := int(header[1] & 0x7F)
	switch size {
	case 126:
		extended := make([]byte, 2)
		_, err = io.ReadFull(conn, extended)
		assert.Nil(tb, err)
		size = int(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		_, err = io.ReadFull(conn, extended)
		assert.Nil(tb, err)
		size = int(binary.BigEndian.Uint64(extended))
	}
	payload = make([]byte, size)
	_, err = io.ReadFull(conn, payload)
	assert.Nil(tb, err)
	return compressed, payload
}

// bodyFor frames a proto message as one B message.
func bodyFor(tb testing.TB, message proto.Message) []byte {
	tb.Helper()
	encoded, err := proto.Marshal(message)
	assert.Nil(tb, err)
	return append([]byte{wireBody}, encoded...)
}

// pingOverRawFrames sends one unary Ping and reports whether the server
// compressed its response.
func pingOverRawFrames(tb testing.TB, text string, options ...connectwebsocket.ServerOption) bool {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(
		connectwebsocket.Mux(mux, server, options...),
		server,
	)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)

	addr := httpServer.Listener.Addr().String()
	ctx, cancel := context.WithTimeout(tb.Context(), 20*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	assert.Nil(tb, err)
	tb.Cleanup(func() { _ = conn.Close() })
	_ = rawHandshake(tb, conn, addr, pingUnaryProcedure)

	assert.Nil(tb, conn.SetDeadline(time.Now().Add(20*time.Second)))
	// Every stream opens with Leading-Metadata before anything else.
	assert.Nil(tb, writeClientFrame(conn, openMessage(), false, true))
	// The request goes uncompressed: RSV1 is per message, so a peer may send
	// plain frames even once the extension is negotiated.
	assert.Nil(tb, writeClientFrame(conn, bodyFor(tb, &pingv1.PingRequest{Text: text}), false, false))

	// Past the server's opening M, which is tiny and says nothing about how
	// the response body was framed.
	_, opening := readServerFrame(tb, conn)
	assert.Equal(tb, opening[0], wireMetadata)

	compressed, payload := readServerFrame(tb, conn)
	// The response must be the echoed message, not the terminal one. Its
	// marker is unreadable here when the frame is compressed, which is the
	// case under test, so length is all this can check.
	assert.True(tb, len(payload) > 0)
	return compressed
}

// openMessage is the empty M message that starts a stream.
func openMessage() []byte {
	return append([]byte{wireMetadata}, []byte("{}")...)
}

// A response comfortably over the threshold must actually go out compressed.
func TestServerCompressesLargeResponse(t *testing.T) {
	t.Parallel()
	assert.True(t, pingOverRawFrames(t, strings.Repeat("compress me ", 1024)))
}

// One comfortably under it must not: deflate on a tiny payload costs bytes.
func TestServerSkipsCompressionBelowThreshold(t *testing.T) {
	t.Parallel()
	assert.False(t, pingOverRawFrames(t, "tiny"))
}

// The threshold is the knob that decides, so raising it past a payload that
// would otherwise be compressed must turn compression off for it.
func TestCompressMinBytesMovesTheThreshold(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("compress me ", 1024) // ~12KiB, compressed by default
	assert.False(t, pingOverRawFrames(t, text, connectwebsocket.WithCompressMinBytes(1<<20)))
}

// And WithoutCompression must stop it regardless of size.
func TestWithoutCompressionLeavesRSV1Clear(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("compress me ", 1024)
	assert.False(t, pingOverRawFrames(t, text, connectwebsocket.WithoutCompression()))
}

// A marker that belongs to the other direction is a different mistake from one
// nobody has defined, and the peer can only correct what it is told. S is the
// server's to send; a client sending it has the roles backwards, which
// "unknown marker" would not convey.
func TestWrongDirectionMarkerIsNamedAsSuch(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		marker byte
		want   string
	}{
		{"end-stream is server-only", wireServerEndStream, "only a server may send"},
		{"nothing recognized is unknown", 'Z', "unknown marker Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer(t, pingServer{})
			conn := dialCumSum(t, httpServer, "")
			sendJSONMessage(t, conn, test.marker, []byte(`{}`))

			data := readServerOpening(t, conn)
			assert.True(t, strings.Contains(string(data), test.want))
			assert.True(t, strings.Contains(string(data), "invalid_argument"))
		})
	}
}

// A server that negotiates permessage-deflate without no-context-takeover is
// refused. The server is required not to do it, but a client that trusted the
// server to be conforming would be protected only as far as the peer is — and
// the plaintext leaking across messages would be the client's own.
func TestClientRefusesSharedCompressionContext(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		extension string
		refuse    bool
	}{
		{
			name:      "both directions bounded",
			extension: "permessage-deflate; client_no_context_takeover; server_no_context_takeover",
		},
		{
			name:      "no extension at all",
			extension: "",
		},
		{
			name:      "plain deflate",
			extension: "permessage-deflate",
			refuse:    true,
		},
		{
			name:      "only the server's side bounded",
			extension: "permessage-deflate; server_no_context_takeover",
			refuse:    true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := httptest.NewServer(http.HandlerFunc(
				func(responseWriter http.ResponseWriter, request *http.Request) {
					handshakeWithExtension(t, responseWriter, request, test.extension)
				},
			))
			t.Cleanup(httpServer.Close)

			transport, err := connectwebsocket.NewTransport(
				httpServer.URL,
				connectwebsocket.WithHTTPClient(httpServer.Client()),
			)
			assert.Nil(t, err)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
			stream, err := client.CumSum(t.Context())
			assert.Nil(t, err)
			sendErr := stream.Send(&pingv1.CumSumRequest{Number: 1})

			if !test.refuse {
				assert.Nil(t, sendErr)
				assert.Nil(t, stream.CloseSend())
				return
			}
			assert.NotNil(t, sendErr)
			assert.True(t, strings.Contains(sendErr.Error(), "no-context-takeover"))
		})
	}
}

// handshakeWithExtension completes an upgrade by hand, echoing an arbitrary
// Sec-WebSocket-Extensions value that coder's own server would never produce.
func handshakeWithExtension(
	tb testing.TB,
	responseWriter http.ResponseWriter,
	request *http.Request,
	extension string,
) {
	tb.Helper()
	hijacker, ok := responseWriter.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	sum := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + websocketGUID))
	response := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n" +
		"Sec-WebSocket-Protocol: " + request.Header.Get("Sec-WebSocket-Protocol") + "\r\n"
	if extension != "" {
		response += "Sec-WebSocket-Extensions: " + extension + "\r\n"
	}
	if _, err := conn.Write([]byte(response + "\r\n")); err != nil {
		return
	}
	// Hold the connection open so the client's own verdict is what the test
	// observes, not a closed socket.
	buffer := make([]byte, 1024)
	for {
		if _, err := conn.Read(buffer); err != nil {
			return
		}
	}
}

// The high bit is reserved for a later revision of the protocol, so a first
// byte at or above 0x80 is refused before anything behind it is interpreted.
// Holding it back is only worth anything if this revision actually rejects it:
// a peer that accepted 0x80 today would make the signal unusable tomorrow.
func TestMarkerHighBitIsReserved(t *testing.T) {
	t.Parallel()
	for _, first := range []byte{0x80, 0xC3, 0xF0, 0xFF} {
		t.Run(fmt.Sprintf("0x%02x", first), func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer(t, pingServer{})
			conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
			// Written as raw bytes: encoding this through a string would
			// UTF-8 it into two bytes and test something else entirely.
			assert.Nil(t, conn.Write(t.Context(), websocket.MessageBinary,
				append([]byte{first}, 0x01, 0x02)))

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			assert.True(t, strings.Contains(string(data), "invalid_argument"))
			assert.True(t, strings.Contains(string(data), "reserved high bit"))
		})
	}
}

// Every value below the high bit is a marker this revision has not assigned,
// and an unassigned one is an unknown marker rather than something to skip.
func TestUnassignedMarkersAreUnknown(t *testing.T) {
	t.Parallel()
	for _, marker := range []byte{0x00, 0x01, 'A', 'Z', 0x7F} {
		t.Run(fmt.Sprintf("0x%02x", marker), func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer(t, pingServer{})
			conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
			assert.Nil(t, conn.Write(t.Context(), websocket.MessageBinary,
				append([]byte{marker}, 0x01)))

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			assert.True(t, strings.Contains(string(data), "invalid_argument"))
		})
	}
}
