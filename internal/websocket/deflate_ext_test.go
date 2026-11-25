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

package websocket_test

import (
	"bufio"
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect/v2/internal/assert"
	"connectrpc.com/connect/v2/internal/memhttp"
	"connectrpc.com/connect/v2/internal/memhttp/memhttptest"
	"connectrpc.com/connect/v2/internal/websocket"
)

const (
	rsv1Bit         = 0x40
	deflateAccepted = "permessage-deflate; server_no_context_takeover; client_no_context_takeover"
	offerHeader     = "Sec-WebSocket-Extensions: permessage-deflate\r\n"
)

func TestCompressionModes(t *testing.T) {
	t.Parallel()
	for _, serverOn := range []bool{false, true} {
		for _, clientOn := range []bool{false, true} {
			t.Run(fmt.Sprintf("server_%t/client_%t", serverOn, clientOn), func(t *testing.T) {
				t.Parallel()
				server := newServerWith(t, websocket.Options{Subprotocol: testSubprotocol, Compression: serverOn}, echo)
				options := websocket.Options{Subprotocol: testSubprotocol, Compression: clientOn}
				conn, response, err := websocket.Dial(&http.Client{Transport: server.TransportHTTP1()}, newRequest(t, server.URL()), options) //nolint:bodyclose // Dial set it to http.NoBody
				assert.Nil(t, err)
				t.Cleanup(func() { _ = conn.Close() })
				wantExtensions := ""
				if serverOn && clientOn {
					wantExtensions = deflateAccepted
				}
				assert.Equal(t, response.Header.Get("Sec-WebSocket-Extensions"), wantExtensions)
				var dst bytes.Buffer
				for _, size := range []int{0, 1, 125, 126, 511, 512, 513, 65535, 65536, 70000, 1 << 20} {
					for _, messageType := range []websocket.MessageType{websocket.MessageText, websocket.MessageBinary} {
						data := strings.Repeat("é", size/2)
						if size%2 == 1 {
							data += "x"
						}
						assert.Nil(t, conn.WriteMessage(messageType, frameOf(data)))
						dst.Reset()
						gotType, err := conn.ReadMessage(&dst, int64(size))
						assert.Nil(t, err)
						assert.Equal(t, gotType, messageType)
						assert.Equal(t, dst.Len(), size)
						assert.True(t, dst.String() == data, assert.Sprintf("size %d", size))
					}
				}
			})
		}
	}
}

func TestCompressionHandshake(t *testing.T) {
	t.Parallel()
	server := newServerWith(t, websocket.Options{Subprotocol: testSubprotocol, Compression: true}, echo)
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "bare", header: offerHeader, want: deflateAccepted},
		{name: "window_bits_15", header: "Sec-WebSocket-Extensions: permessage-deflate; server_max_window_bits=15\r\n", want: deflateAccepted + "; server_max_window_bits=15"},
		{name: "two_lines", header: "Sec-WebSocket-Extensions: permessage-deflate; server_max_window_bits=10\r\nSec-WebSocket-Extensions: permessage-deflate\r\n", want: deflateAccepted},
		{name: "declined", header: "Sec-WebSocket-Extensions: permessage-deflate; server_max_window_bits=10\r\n"},
		{name: "malformed", header: "Sec-WebSocket-Extensions: permessage-deflate;;\r\n"},
		{name: "none"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			netConn, _, header := rawDialHeader(t, server, test.header, nil)
			assert.Equal(t, header.Values("Sec-WebSocket-Extensions"), headerValues(test.want))
			_ = netConn.Close()
		})
	}
}

func TestDialCompression(t *testing.T) {
	t.Parallel()
	t.Run("offer", func(t *testing.T) {
		t.Parallel()
		offers := make(chan []string, 2)
		server := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			offers <- request.Header.Values("Sec-WebSocket-Extensions")
			http.Error(response, "no", http.StatusTeapot)
		}))
		httpClient := &http.Client{Transport: server.TransportHTTP1()}
		request := newRequest(t, server.URL())
		request.Header.Set("Sec-WebSocket-Extensions", "x-custom")
		_, response, err := websocket.Dial(httpClient, request, websocket.Options{Compression: true})
		assert.NotNil(t, err)
		_ = response.Body.Close()
		assert.Equal(t, <-offers, []string{"permessage-deflate; client_no_context_takeover; server_no_context_takeover"})
		_, response, err = websocket.Dial(httpClient, request, websocket.Options{})
		assert.NotNil(t, err)
		_ = response.Body.Close()
		assert.Equal(t, <-offers, []string(nil))
	})
	tests := []struct {
		name        string
		extensions  string
		compression bool
		wantErr     bool
	}{
		{name: "none", compression: true},
		{name: "both", extensions: deflateAccepted, compression: true},
		{name: "server_only", extensions: "permessage-deflate; server_no_context_takeover", compression: true},
		{name: "window_bits", extensions: deflateAccepted + "; server_max_window_bits=10", compression: true},
		{name: "bare", extensions: "permessage-deflate", compression: true, wantErr: true},
		{name: "client_only", extensions: "permessage-deflate; client_no_context_takeover", compression: true, wantErr: true},
		{name: "client_window_bits", extensions: deflateAccepted + "; client_max_window_bits=15", compression: true, wantErr: true},
		{name: "unknown", extensions: "x-webkit-deflate-frame", compression: true, wantErr: true},
		{name: "unrequested", extensions: deflateAccepted, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			received := make(chan []byte, 1)
			mutate := func(header http.Header) {
				if test.extensions != "" {
					header.Set("Sec-WebSocket-Extensions", test.extensions)
				}
			}
			server := memhttptest.NewServer(t, rawUpgradeHandler(mutate, func(netConn net.Conn, reader *bufio.Reader) {
				if test.wantErr {
					_, _ = io.Copy(io.Discard, reader)
					return
				}
				// A compressed "Hello" from RFC 7692 Section 7.2.3.1, which
				// the client can read only with compression on.
				_, _ = netConn.Write([]byte{0xc1, 0x07, 0xf2, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00})
				first, payload := readRawFrame(t, reader)
				received <- append([]byte{first}, payload...)
				_, _ = io.Copy(io.Discard, reader)
			}))
			options := websocket.Options{Subprotocol: testSubprotocol, Compression: test.compression}
			conn, response, err := websocket.Dial(&http.Client{Transport: server.TransportHTTP1()}, newRequest(t, server.URL()), options) //nolint:bodyclose // Dial set it to http.NoBody
			assert.Equal(t, response.StatusCode, http.StatusSwitchingProtocols)
			if test.wantErr {
				assert.NotNil(t, err)
				return
			}
			assert.Nil(t, err)
			defer func() { _ = conn.Close() }()
			assert.Equal(t, response.Header.Get("Sec-WebSocket-Extensions"), test.extensions)
			var dst bytes.Buffer
			_, err = conn.ReadMessage(&dst, -1)
			if test.extensions == "" {
				assert.ErrorIs(t, err, websocket.ErrProtocol)
				return
			}
			assert.Nil(t, err)
			assert.Equal(t, dst.String(), "Hello")
			data := strings.Repeat("compressible ", 100)
			assert.Nil(t, conn.WriteMessage(websocket.MessageText, frameOf(data)))
			frame := <-received
			assert.Equal(t, frame[0], finBit|rsv1Bit|opText)
			assert.Equal(t, inflate(t, frame[1:]), data)
		})
	}
}

func TestCompressedFragments(t *testing.T) {
	t.Parallel()
	server := newServerWith(t, websocket.Options{Subprotocol: testSubprotocol, Compression: true}, echo)
	netConn, reader, _ := rawDialHeader(t, server, offerHeader, nil)
	compressed := deflate(t, []byte("héllo"))
	var frames []byte
	frames = appendFrame(frames, rsv1Bit|opText, compressed[:2], true)
	frames = appendFrame(frames, finBit|opPing, []byte("ping"), true)
	frames = appendFrame(frames, opCont, compressed[2:3], true)
	frames = appendFrame(frames, opCont, nil, true)
	frames = appendFrame(frames, finBit|opCont, compressed[3:], true)
	go func() { _, _ = netConn.Write(frames) }()
	first, payload := readRawFrame(t, reader)
	assert.Equal(t, first, finBit|opPong)
	assert.Equal(t, string(payload), "ping")
	// Below the threshold the echo goes uncompressed.
	first, payload = readRawFrame(t, reader)
	assert.Equal(t, first, finBit|opText)
	assert.Equal(t, string(payload), "héllo")
}

func TestCompressionThreshold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		threshold      int
		data           string
		wantCompressed bool
	}{
		{name: "default_below", data: strings.Repeat("a", 511)},
		{name: "default_at", data: strings.Repeat("a", 512), wantCompressed: true},
		{name: "custom", threshold: 50, data: strings.Repeat("a", 100), wantCompressed: true},
		{name: "incompressible", threshold: 1, data: "abc"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := websocket.Options{Subprotocol: testSubprotocol, Compression: true, CompressionThreshold: test.threshold}
			server := newServerWith(t, options, echo)
			netConn, reader, _ := rawDialHeader(t, server, offerHeader, nil)
			go func() { _, _ = netConn.Write(appendFrame(nil, finBit|opText, []byte(test.data), true)) }()
			first, payload := readRawFrame(t, reader)
			assert.Equal(t, first&rsv1Bit != 0, test.wantCompressed)
			if test.wantCompressed {
				payload = []byte(inflate(t, payload))
			}
			assert.Equal(t, string(payload), test.data)
		})
	}
}

func TestCompressedProtocolErrors(t *testing.T) {
	t.Parallel()
	const readLimit = 64 << 10
	compressedFrame := func(first byte, data []byte) []byte { return appendFrame(nil, first, deflate(t, data), true) }
	tests := []struct {
		name       string
		noOffer    bool
		frames     [][]byte
		wantStatus websocket.StatusCode
		wantErr    error
	}{
		{name: "not_negotiated", noOffer: true, frames: [][]byte{compressedFrame(finBit|rsv1Bit|opText, []byte("a"))}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "rsv1_continuation", frames: [][]byte{appendFrame(nil, rsv1Bit|opText, []byte{0xf2}, true), appendFrame(nil, finBit|rsv1Bit|opCont, []byte{0x00}, true)}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "rsv1_ping", frames: [][]byte{appendFrame(nil, finBit|rsv1Bit|opPing, nil, true)}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "rsv2", frames: [][]byte{appendFrame(nil, finBit|0x20|opText, []byte("a"), true)}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "corrupt", frames: [][]byte{appendFrame(nil, finBit|rsv1Bit|opBinary, []byte{0xff, 0xff}, true)}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "invalid_utf8", frames: [][]byte{compressedFrame(finBit|rsv1Bit|opText, []byte("ok\xff"))}, wantStatus: websocket.StatusInvalidPayloadData, wantErr: websocket.ErrInvalidUTF8},
		{name: "zeros", frames: [][]byte{compressedFrame(finBit|rsv1Bit|opBinary, make([]byte, 1<<20))}, wantStatus: websocket.StatusMessageTooBig, wantErr: websocket.ErrMessageTooBig},
		{name: "empty_block_flood", frames: [][]byte{appendFrame(nil, finBit|rsv1Bit|opBinary, bytes.Repeat([]byte{0x00, 0x00, 0x00, 0xff, 0xff}, 30<<10), true)}, wantStatus: websocket.StatusMessageTooBig, wantErr: websocket.ErrMessageTooBig},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			readErrs := make(chan [2]error, 1)
			server := newServerWith(t, websocket.Options{Subprotocol: testSubprotocol, Compression: true}, func(conn *websocket.Conn) {
				var dst bytes.Buffer
				var errs [2]error
				for errs[0] == nil {
					dst.Reset()
					_, errs[0] = conn.ReadMessage(&dst, readLimit)
				}
				_, errs[1] = conn.ReadMessage(&dst, readLimit)
				readErrs <- errs
			})
			header := offerHeader
			if test.noOffer {
				header = ""
			}
			netConn, reader, _ := rawDialHeader(t, server, header, nil)
			go func() {
				for _, frame := range test.frames {
					if _, err := netConn.Write(frame); err != nil {
						return
					}
				}
			}()
			code, _ := readCloseFrame(t, reader)
			assert.Equal(t, code, test.wantStatus)
			_, _ = netConn.Write(appendFrame(nil, finBit|opClose, closePayload(code, ""), true))
			_, _ = io.Copy(io.Discard, reader)
			errs := <-readErrs
			assert.ErrorIs(t, errs[0], test.wantErr)
			assert.ErrorIs(t, errs[1], errs[0])
		})
	}
}

func newServerWith(t *testing.T, options websocket.Options, handle func(*websocket.Conn)) *memhttp.Server {
	t.Helper()
	return memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(response, request, options)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		handle(conn)
	}))
}

// deflate compresses data as RFC 7692 Section 7.2.1 describes.
func deflate(t *testing.T, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := flate.NewWriter(&output, flate.BestSpeed)
	assert.Nil(t, err)
	_, err = writer.Write(data)
	assert.Nil(t, err)
	assert.Nil(t, writer.Flush())
	return bytes.TrimSuffix(output.Bytes(), []byte{0x00, 0x00, 0xff, 0xff})
}

// inflate reverses deflate.
func inflate(t *testing.T, payload []byte) string {
	t.Helper()
	source := io.MultiReader(bytes.NewReader(payload), bytes.NewReader([]byte{0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff}))
	output, err := io.ReadAll(flate.NewReader(source))
	assert.Nil(t, err)
	return string(output)
}

func headerValues(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}
