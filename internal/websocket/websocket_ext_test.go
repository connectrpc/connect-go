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
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"connectrpc.com/connect/v2/internal/assert"
	"connectrpc.com/connect/v2/internal/memhttp"
	"connectrpc.com/connect/v2/internal/memhttp/memhttptest"
	"connectrpc.com/connect/v2/internal/websocket"
)

const (
	testSubprotocol = "test"
	// testKey and testAccept are the example in RFC 6455 Section 1.3.
	testKey    = "dGhlIHNhbXBsZSBub25jZQ=="
	testAccept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="

	finBit   = 0x80
	opCont   = 0x0
	opText   = 0x1
	opBinary = 0x2
	opClose  = 0x8
	opPing   = 0x9
	opPong   = 0xa
)

func TestAccept(t *testing.T) {
	t.Parallel()
	server := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(response, request, &websocket.AcceptOptions{Subprotocols: []string{"b", "a"}})
		if err == nil {
			_ = conn.CloseNow()
		}
	}))
	host := strings.TrimPrefix(server.URL(), "http://")
	tests := []struct {
		name         string
		method       string
		header       map[string]string
		http2        bool
		wantStatus   int
		wantProtocol string
	}{
		{name: "valid", wantStatus: http.StatusSwitchingProtocols},
		{name: "same_origin", header: map[string]string{"Origin": "http://" + strings.ToUpper(host)}, wantStatus: http.StatusSwitchingProtocols},
		{name: "cross_origin", header: map[string]string{"Origin": "http://evil.example"}, wantStatus: http.StatusForbidden},
		{name: "null_origin", header: map[string]string{"Origin": "null"}, wantStatus: http.StatusForbidden},
		{name: "post", method: http.MethodPost, wantStatus: http.StatusMethodNotAllowed},
		{name: "no_upgrade", header: map[string]string{"Upgrade": ""}, wantStatus: http.StatusUpgradeRequired},
		{name: "no_connection", header: map[string]string{"Connection": "keep-alive"}, wantStatus: http.StatusUpgradeRequired},
		{name: "http2", http2: true, wantStatus: http.StatusUpgradeRequired},
		{name: "version_12", header: map[string]string{"Sec-WebSocket-Version": "12"}, wantStatus: http.StatusBadRequest},
		{name: "no_key", header: map[string]string{"Sec-WebSocket-Key": ""}, wantStatus: http.StatusBadRequest},
		{name: "short_key", header: map[string]string{"Sec-WebSocket-Key": "c2hvcnQ="}, wantStatus: http.StatusBadRequest},
		{name: "subprotocol", header: map[string]string{"Sec-WebSocket-Protocol": "x, A, b"}, wantStatus: http.StatusSwitchingProtocols, wantProtocol: "b"},
		{name: "subprotocol_spelling", header: map[string]string{"Sec-WebSocket-Protocol": "x, A"}, wantStatus: http.StatusSwitchingProtocols, wantProtocol: "A"},
		{name: "subprotocol_unsupported", header: map[string]string{"Sec-WebSocket-Protocol": "x"}, wantStatus: http.StatusSwitchingProtocols},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			method := http.MethodGet
			if test.method != "" {
				method = test.method
			}
			request, err := http.NewRequestWithContext(t.Context(), method, server.URL(), http.NoBody)
			assert.Nil(t, err)
			request.Header.Set("Sec-WebSocket-Version", "13")
			request.Header.Set("Sec-WebSocket-Key", testKey)
			transport := server.Transport()
			if !test.http2 {
				transport = server.TransportHTTP1()
				request.Header.Set("Connection", "Upgrade")
				request.Header.Set("Upgrade", "websocket")
			}
			for key, value := range test.header {
				if value == "" {
					request.Header.Del(key)
				} else {
					request.Header.Set(key, value)
				}
			}
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatalf("round trip: %v", err)
			}
			_ = response.Body.Close()
			assert.Equal(t, response.StatusCode, test.wantStatus)
			if test.wantStatus == http.StatusSwitchingProtocols {
				assert.Equal(t, response.Header.Get("Sec-WebSocket-Accept"), testAccept)
				assert.Equal(t, response.Header.Get("Sec-WebSocket-Protocol"), test.wantProtocol)
			}
		})
	}
}

func TestAcceptBufferedFrame(t *testing.T) {
	t.Parallel()
	server := newServer(t, echo)
	// The frame arrives with the handshake, buffered by net/http.
	netConn, reader := rawDial(t, server, appendFrame(nil, finBit|opText, []byte("early"), true))
	first, payload := readRawFrame(t, reader)
	assert.Equal(t, first, finBit|opText)
	assert.Equal(t, string(payload), "early")
	_ = netConn.Close()
}

func TestDial(t *testing.T) {
	t.Parallel()
	t.Run("subprotocol", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			conn, err := websocket.Accept(response, request, &websocket.AcceptOptions{Subprotocols: []string{"b"}})
			if err == nil {
				_ = conn.CloseNow()
			}
		}))
		conn := dial(t, server, &websocket.DialOptions{Subprotocols: []string{"a", "b"}})
		assert.Equal(t, conn.Subprotocol(), "b")
	})
	t.Run("ws_scheme", func(t *testing.T) {
		t.Parallel()
		server := newServer(t, echo)
		conn, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL(), "http"), &websocket.DialOptions{ //nolint:bodyclose // no body on success
			HTTPClient: &http.Client{Transport: server.TransportHTTP1()},
		})
		assert.Nil(t, err)
		assert.Nil(t, response.Body)
		assert.Nil(t, conn.CloseNow())
	})
	t.Run("http_error", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			http.Error(response, "nope", http.StatusTeapot)
		}))
		_, response, err := websocket.Dial(t.Context(), server.URL(), &websocket.DialOptions{
			HTTPClient: &http.Client{Transport: server.TransportHTTP1()},
		})
		assert.NotNil(t, err)
		assert.Equal(t, response.StatusCode, http.StatusTeapot)
		body, readErr := io.ReadAll(response.Body)
		assert.Nil(t, readErr)
		assert.Equal(t, string(body), "nope\n")
		assert.Nil(t, response.Body.Close())
	})
	t.Run("unsupported_scheme", func(t *testing.T) {
		t.Parallel()
		_, response, err := websocket.Dial(t.Context(), "ftp://example.com", nil) //nolint:bodyclose // no body on success
		assert.NotNil(t, err)
		assert.Nil(t, response)
	})
	tests := []struct {
		name    string
		mutate  func(http.Header)
		wantErr bool
	}{
		{name: "valid", mutate: func(http.Header) {}},
		{name: "no_subprotocol", mutate: func(header http.Header) { header.Del("Sec-WebSocket-Protocol") }},
		{name: "wrong_accept", mutate: func(header http.Header) { header.Set("Sec-WebSocket-Accept", testAccept) }, wantErr: true},
		{name: "no_upgrade", mutate: func(header http.Header) { header.Del("Upgrade") }, wantErr: true},
		{name: "no_connection", mutate: func(header http.Header) { header.Set("Connection", "close") }, wantErr: true},
		{name: "extension", mutate: func(header http.Header) { header.Set("Sec-WebSocket-Extensions", "permessage-deflate") }, wantErr: true},
		{name: "unoffered_subprotocol", mutate: func(header http.Header) { header.Set("Sec-WebSocket-Protocol", "other") }, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := memhttptest.NewServer(t, rawUpgradeHandler(test.mutate, func(netConn net.Conn, _ *bufio.Reader) {
				_, _ = io.Copy(io.Discard, netConn)
			}))
			conn, response, err := websocket.Dial(t.Context(), server.URL(), &websocket.DialOptions{
				HTTPClient:   &http.Client{Transport: server.TransportHTTP1()},
				Subprotocols: []string{testSubprotocol},
			})
			assert.Equal(t, response.StatusCode, http.StatusSwitchingProtocols)
			if test.wantErr {
				assert.NotNil(t, err)
				assert.Nil(t, response.Body.Close())
				return
			}
			assert.Nil(t, err)
			assert.Nil(t, response.Body)
			assert.Equal(t, conn.Subprotocol(), response.Header.Get("Sec-WebSocket-Protocol"))
			assert.Nil(t, conn.CloseNow())
		})
	}
}

func TestDialTimeoutBoundsHandshake(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		server := newServer(t, echo)
		conn := dial(t, server, &websocket.DialOptions{
			HTTPClient: &http.Client{Transport: server.TransportHTTP1(), Timeout: time.Second},
		})
		time.Sleep(2 * time.Second)
		assert.Nil(t, conn.Write(t.Context(), websocket.MessageText, []byte("late")))
		_, data, err := conn.Read(t.Context())
		assert.Nil(t, err)
		assert.Equal(t, string(data), "late")
	})
}

func TestMessageSizes(t *testing.T) {
	t.Parallel()
	server := newServer(t, func(ctx context.Context, conn *websocket.Conn) {
		conn.SetReadLimit(-1)
		echo(ctx, conn)
	})
	conn := dial(t, server, nil)
	conn.SetReadLimit(-1)
	for _, size := range []int{0, 1, 125, 126, 127, 65535, 65536, 70000} {
		for _, messageType := range []websocket.MessageType{websocket.MessageText, websocket.MessageBinary} {
			data := bytes.Repeat([]byte("é"), size/2)
			if size%2 == 1 {
				data = append(data, 'x')
			}
			assert.Nil(t, conn.Write(t.Context(), messageType, data))
			gotType, got, err := conn.Read(t.Context())
			assert.Nil(t, err)
			assert.Equal(t, gotType, messageType)
			assert.Equal(t, len(got), size)
			assert.True(t, bytes.Equal(got, data), assert.Sprintf("size %d", size))
		}
	}
}

func TestReader(t *testing.T) {
	t.Parallel()
	server := newServer(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = conn.Write(ctx, websocket.MessageText, []byte("first"))
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("second"))
		_, _, _ = conn.Read(ctx)
	})
	conn := dial(t, server, nil)
	messageType, reader, err := conn.Reader(t.Context())
	assert.Nil(t, err)
	assert.Equal(t, messageType, websocket.MessageText)
	_, _, err = conn.Reader(t.Context())
	assert.NotNil(t, err)
	data, err := io.ReadAll(iotest.OneByteReader(reader))
	assert.Nil(t, err)
	assert.Equal(t, string(data), "first")
	messageType, data, err = conn.Read(t.Context())
	assert.Nil(t, err)
	assert.Equal(t, messageType, websocket.MessageBinary)
	assert.Equal(t, string(data), "second")
}

func TestFragmentedMessage(t *testing.T) {
	t.Parallel()
	server := newServer(t, echo)
	netConn, reader := rawDial(t, server, nil)
	var frames []byte
	frames = appendFrame(frames, opText, []byte("h\xc3"), true)
	frames = appendFrame(frames, finBit|opPing, []byte("ping"), true)
	frames = appendFrame(frames, opCont, []byte("\xa9l"), true)
	frames = appendFrame(frames, finBit|opPong, []byte("unsolicited"), true)
	frames = appendFrame(frames, opCont, nil, true)
	frames = appendFrame(frames, finBit|opCont, []byte("lo"), true)
	go func() { _, _ = netConn.Write(frames) }()
	first, payload := readRawFrame(t, reader)
	assert.Equal(t, first, finBit|opPong)
	assert.Equal(t, string(payload), "ping")
	first, payload = readRawFrame(t, reader)
	assert.Equal(t, first, finBit|opText)
	assert.Equal(t, string(payload), "héllo")
}

func TestProtocolErrors(t *testing.T) {
	t.Parallel()
	const readLimit = 16
	textFrame := func(first byte, payload string) []byte { return appendFrame(nil, first, []byte(payload), true) }
	closeFrame := func(payload []byte) []byte { return appendFrame(nil, finBit|opClose, payload, true) }
	tests := []struct {
		name       string
		frames     [][]byte
		wantStatus websocket.StatusCode
		tooBig     bool
		badUTF8    bool
	}{
		{name: "rsv1", frames: [][]byte{textFrame(finBit|0x40|opText, "a")}, wantStatus: websocket.StatusProtocolError},
		{name: "opcode_3", frames: [][]byte{textFrame(finBit|0x3, "a")}, wantStatus: websocket.StatusProtocolError},
		{name: "opcode_11", frames: [][]byte{textFrame(finBit|0xb, "")}, wantStatus: websocket.StatusProtocolError},
		{name: "unmasked", frames: [][]byte{appendFrame(nil, finBit|opText, []byte("a"), false)}, wantStatus: websocket.StatusProtocolError},
		{name: "long_ping", frames: [][]byte{textFrame(finBit|opPing, strings.Repeat("p", 126))}, wantStatus: websocket.StatusProtocolError},
		{name: "fragmented_ping", frames: [][]byte{textFrame(opPing, "p")}, wantStatus: websocket.StatusProtocolError},
		{name: "lone_continuation", frames: [][]byte{textFrame(finBit|opCont, "a")}, wantStatus: websocket.StatusProtocolError},
		{name: "interleaved_message", frames: [][]byte{textFrame(opText, "a"), textFrame(finBit|opText, "b")}, wantStatus: websocket.StatusProtocolError},
		{name: "length_msb", frames: [][]byte{{finBit | opBinary, 0xff, 0x80, 0, 0, 0, 0, 0, 0, 1}}, wantStatus: websocket.StatusProtocolError},
		{name: "close_one_byte", frames: [][]byte{closeFrame([]byte{3})}, wantStatus: websocket.StatusProtocolError},
		{name: "close_999", frames: [][]byte{closeFrame(closePayload(999, ""))}, wantStatus: websocket.StatusProtocolError},
		{name: "close_1004", frames: [][]byte{closeFrame(closePayload(1004, ""))}, wantStatus: websocket.StatusProtocolError},
		{name: "close_1005", frames: [][]byte{closeFrame(closePayload(1005, ""))}, wantStatus: websocket.StatusProtocolError},
		{name: "close_1006", frames: [][]byte{closeFrame(closePayload(1006, ""))}, wantStatus: websocket.StatusProtocolError},
		{name: "close_1015", frames: [][]byte{closeFrame(closePayload(1015, ""))}, wantStatus: websocket.StatusProtocolError},
		{name: "close_2999", frames: [][]byte{closeFrame(closePayload(2999, ""))}, wantStatus: websocket.StatusProtocolError},
		{name: "close_5000", frames: [][]byte{closeFrame(closePayload(5000, ""))}, wantStatus: websocket.StatusProtocolError},
		{name: "close_reason_utf8", frames: [][]byte{closeFrame(closePayload(1000, "\xff"))}, wantStatus: websocket.StatusInvalidFramePayloadData},
		{name: "text_utf8", frames: [][]byte{textFrame(finBit|opText, "a\xffb")}, wantStatus: websocket.StatusInvalidFramePayloadData, badUTF8: true},
		{name: "text_ends_in_rune", frames: [][]byte{textFrame(opText, "a"), textFrame(finBit|opCont, "\xc3")}, wantStatus: websocket.StatusInvalidFramePayloadData, badUTF8: true},
		{name: "too_big", frames: [][]byte{textFrame(finBit|opBinary, strings.Repeat("x", readLimit+1))}, wantStatus: websocket.StatusMessageTooBig, tooBig: true},
		{name: "too_big_fragments", frames: [][]byte{textFrame(opBinary, "0123456789"), textFrame(finBit|opCont, "0123456789")}, wantStatus: websocket.StatusMessageTooBig, tooBig: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			readErr := make(chan error, 1)
			server := newServer(t, func(ctx context.Context, conn *websocket.Conn) {
				conn.SetReadLimit(readLimit)
				readErr <- readUntilError(ctx, conn)
				_ = conn.Close(websocket.StatusNormalClosure, "")
			})
			netConn, reader := rawDial(t, server, nil)
			go func() {
				for _, frame := range test.frames {
					if _, err := netConn.Write(frame); err != nil {
						return
					}
				}
			}()
			code, _ := readCloseFrame(t, reader)
			assert.Equal(t, code, test.wantStatus)
			err := <-readErr
			// The code this side sent isn't the peer's Close.
			assert.Equal(t, websocket.CloseStatus(err), -1, assert.Sprintf("error %v", err))
			assert.Equal(t, errors.Is(err, websocket.ErrMessageTooBig), test.tooBig)
			assert.Equal(t, errors.Is(err, websocket.ErrInvalidUTF8), test.badUTF8)
		})
	}
}

func TestReadLimit(t *testing.T) {
	t.Parallel()
	const defaultLimit = 32768
	t.Run("default", func(t *testing.T) {
		t.Parallel()
		readErr := make(chan error, 1)
		server := newServer(t, func(ctx context.Context, conn *websocket.Conn) {
			_, data, err := conn.Read(ctx)
			if err == nil && len(data) == defaultLimit {
				_, _, err = conn.Read(ctx)
			}
			readErr <- err
			// Close drains the rest of the oversized message.
			_ = conn.Close(websocket.StatusMessageTooBig, "")
		})
		conn := dial(t, server, nil)
		clientErr := make(chan error, 1)
		go func() {
			_, _, err := conn.Read(t.Context())
			clientErr <- err
		}()
		assert.Nil(t, conn.Write(t.Context(), websocket.MessageBinary, make([]byte, defaultLimit)))
		assert.Nil(t, conn.Write(t.Context(), websocket.MessageBinary, make([]byte, 1<<20)))
		assert.Equal(t, websocket.CloseStatus(<-clientErr), websocket.StatusMessageTooBig)
		assert.ErrorIs(t, <-readErr, websocket.ErrMessageTooBig)
	})
	t.Run("unlimited", func(t *testing.T) {
		t.Parallel()
		server := newServer(t, func(ctx context.Context, conn *websocket.Conn) {
			conn.SetReadLimit(-1)
			echo(ctx, conn)
		})
		conn := dial(t, server, nil)
		conn.SetReadLimit(-1)
		assert.Nil(t, conn.Write(t.Context(), websocket.MessageBinary, make([]byte, 1<<20)))
		_, data, err := conn.Read(t.Context())
		assert.Nil(t, err)
		assert.Equal(t, len(data), 1<<20)
	})
}

func TestClientProtocolErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		frame      []byte
		wantStatus websocket.StatusCode
		badUTF8    bool
	}{
		{name: "masked", frame: appendFrame(nil, finBit|opText, []byte("a"), true), wantStatus: websocket.StatusProtocolError},
		{name: "text_utf8", frame: appendFrame(nil, finBit|opText, []byte("M\xff"), false), wantStatus: websocket.StatusInvalidFramePayloadData, badUTF8: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gotStatus := make(chan websocket.StatusCode, 1)
			server := memhttptest.NewServer(t, rawUpgradeHandler(func(http.Header) {}, func(netConn net.Conn, reader *bufio.Reader) {
				_, _ = netConn.Write(test.frame)
				code, _ := readCloseFrame(t, reader)
				gotStatus <- code
			}))
			conn := dial(t, server, nil)
			_, _, err := conn.Read(t.Context())
			// The server didn't close, the client failed the connection.
			assert.Equal(t, websocket.CloseStatus(err), -1, assert.Sprintf("error %v", err))
			assert.Equal(t, errors.Is(err, websocket.ErrInvalidUTF8), test.badUTF8)
			assert.Equal(t, <-gotStatus, test.wantStatus)
		})
	}
}

func TestReceiveClose(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload []byte
		want    websocket.CloseError
	}{
		{name: "normal", payload: closePayload(1000, "bye"), want: websocket.CloseError{Code: 1000, Reason: "bye"}},
		{name: "no_status", want: websocket.CloseError{Code: websocket.StatusNoStatusRcvd}},
		{name: "service_restart", payload: closePayload(1012, ""), want: websocket.CloseError{Code: 1012}},
		{name: "application", payload: closePayload(3000, "é"), want: websocket.CloseError{Code: 3000, Reason: "é"}},
		{name: "private", payload: closePayload(4999, ""), want: websocket.CloseError{Code: 4999}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			readErr := make(chan error, 1)
			server := newServer(t, func(ctx context.Context, conn *websocket.Conn) {
				readErr <- readUntilError(ctx, conn)
			})
			netConn, reader := rawDial(t, server, nil)
			go func() { _, _ = netConn.Write(appendFrame(nil, finBit|opClose, test.payload, true)) }()
			first, echoed := readRawFrame(t, reader)
			assert.Equal(t, first, finBit|opClose)
			assert.True(t, bytes.Equal(echoed, test.payload))
			var closeErr websocket.CloseError
			assert.True(t, errors.As(<-readErr, &closeErr))
			assert.Equal(t, closeErr, test.want)
		})
	}
}

func TestCloseHandshake(t *testing.T) {
	t.Parallel()
	t.Run("client_closes", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			readErr := make(chan error, 1)
			server := newServer(t, func(ctx context.Context, conn *websocket.Conn) {
				readErr <- readUntilError(ctx, conn)
			})
			conn := dial(t, server, nil)
			start := time.Now()
			assert.Nil(t, conn.Close(websocket.StatusNormalClosure, "bye"))
			assert.Equal(t, time.Since(start), 0)
			var closeErr websocket.CloseError
			assert.True(t, errors.As(<-readErr, &closeErr))
			assert.Equal(t, closeErr, websocket.CloseError{Code: websocket.StatusNormalClosure, Reason: "bye"})
			assert.ErrorIs(t, conn.Close(websocket.StatusNormalClosure, ""), net.ErrClosed)
			assert.ErrorIs(t, conn.CloseNow(), net.ErrClosed)
		})
	})
	t.Run("server_closes", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			closeErr := make(chan error, 1)
			server := newServer(t, func(_ context.Context, conn *websocket.Conn) {
				closeErr <- conn.Close(websocket.StatusGoingAway, "bye")
			})
			conn := dial(t, server, nil)
			_, _, err := conn.Read(t.Context())
			assert.Equal(t, websocket.CloseStatus(err), websocket.StatusGoingAway)
			assert.Nil(t, <-closeErr)
			_, _, err = conn.Read(t.Context())
			assert.ErrorIs(t, err, net.ErrClosed)
			assert.ErrorIs(t, conn.Write(t.Context(), websocket.MessageText, nil), net.ErrClosed)
		})
	})
	t.Run("peer_never_echoes", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			server := memhttptest.NewServer(t, rawUpgradeHandler(func(http.Header) {}, func(netConn net.Conn, _ *bufio.Reader) {
				_, _ = io.Copy(io.Discard, netConn)
			}))
			conn := dial(t, server, nil)
			start := time.Now()
			assert.NotNil(t, conn.Close(websocket.StatusNormalClosure, ""))
			assert.Equal(t, time.Since(start), 5*time.Second)
		})
	})
	t.Run("peer_never_reads", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			server := memhttptest.NewServer(t, rawUpgradeHandler(func(http.Header) {}, func(net.Conn, *bufio.Reader) {
				<-release
			}))
			defer close(release)
			conn := dial(t, server, nil)
			start := time.Now()
			assert.NotNil(t, conn.Close(websocket.StatusNormalClosure, ""))
			assert.Equal(t, time.Since(start), 5*time.Second)
		})
	})
	t.Run("close_now_ends_close", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			server := memhttptest.NewServer(t, rawUpgradeHandler(func(http.Header) {}, func(netConn net.Conn, _ *bufio.Reader) {
				_, _ = io.Copy(io.Discard, netConn)
			}))
			conn := dial(t, server, nil)
			start := time.Now()
			closed := make(chan error, 1)
			go func() { closed <- conn.Close(websocket.StatusNormalClosure, "") }()
			time.Sleep(time.Second)
			assert.ErrorIs(t, conn.CloseNow(), net.ErrClosed)
			<-closed
			assert.Equal(t, time.Since(start), time.Second)
		})
	})
	t.Run("concurrent_write", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			server := newServer(t, func(ctx context.Context, conn *websocket.Conn) {
				conn.SetReadLimit(-1)
				_ = readUntilError(ctx, conn)
			})
			conn := dial(t, server, nil)
			writeErr := make(chan error, 1)
			wrote := make(chan struct{})
			go func() {
				for i := 0; ; i++ {
					if err := conn.Write(t.Context(), websocket.MessageBinary, make([]byte, 1<<16)); err != nil {
						writeErr <- err
						return
					}
					if i == 0 {
						close(wrote)
					}
				}
			}()
			<-wrote
			assert.Nil(t, conn.Close(websocket.StatusNormalClosure, ""))
			assert.ErrorIs(t, <-writeErr, net.ErrClosed)
		})
	})
	t.Run("invalid_peer_close", func(t *testing.T) {
		t.Parallel()
		payloads := map[string][]byte{
			"one_byte": {3},
			"too_long": closePayload(websocket.StatusNormalClosure, strings.Repeat("x", 124)),
		}
		for name, payload := range payloads {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					server := newServer(t, func(ctx context.Context, conn *websocket.Conn) {
						_ = readUntilError(ctx, conn)
						_ = conn.Close(websocket.StatusNormalClosure, "")
					})
					netConn, reader := rawDial(t, server, nil)
					start := time.Now()
					_, err := netConn.Write(appendFrame(nil, finBit|opClose, payload, true))
					assert.Nil(t, err)
					code, _ := readCloseFrame(t, reader)
					assert.Equal(t, code, websocket.StatusProtocolError)
					// The peer's Close already arrived, leaving the server no
					// reason to wait before closing the connection.
					_, err = reader.ReadByte()
					assert.ErrorIs(t, err, io.EOF)
					assert.Equal(t, time.Since(start), 0)
				})
			})
		}
	})
	t.Run("invalid_close", func(t *testing.T) {
		t.Parallel()
		server := newServer(t, func(ctx context.Context, conn *websocket.Conn) { _ = readUntilError(ctx, conn) })
		assert.NotNil(t, dial(t, server, nil).Close(websocket.StatusAbnormalClosure, ""))
		assert.NotNil(t, dial(t, server, nil).Close(websocket.StatusNormalClosure, strings.Repeat("x", 124)))
	})
}

func TestPing(t *testing.T) {
	t.Parallel()
	t.Run("pong", func(t *testing.T) {
		t.Parallel()
		server := newServer(t, func(ctx context.Context, conn *websocket.Conn) { _ = readUntilError(ctx, conn) })
		conn := dial(t, server, nil)
		go func() { _ = readUntilError(t.Context(), conn) }()
		for range 3 {
			assert.Nil(t, conn.Ping(t.Context()))
		}
	})
	t.Run("no_pong", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			pingErr := make(chan error, 1)
			server := newServer(t, func(ctx context.Context, conn *websocket.Conn) {
				go func() { _ = readUntilError(ctx, conn) }()
				pingCtx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				pingErr <- conn.Ping(pingCtx)
				// The ended ping left the connection open.
				_ = conn.Write(ctx, websocket.MessageText, []byte("after"))
				_ = conn.Close(websocket.StatusNormalClosure, "")
			})
			pings := 0
			conn := dial(t, server, &websocket.DialOptions{
				OnPingReceived: func(context.Context, []byte) bool {
					pings++
					return false
				},
			})
			_, data, err := conn.Read(t.Context())
			assert.Nil(t, err)
			assert.Equal(t, string(data), "after")
			assert.Equal(t, pings, 1)
			assert.ErrorIs(t, <-pingErr, context.DeadlineExceeded)
		})
	})
	t.Run("closed", func(t *testing.T) {
		t.Parallel()
		server := newServer(t, echo)
		conn := dial(t, server, nil)
		assert.Nil(t, conn.CloseNow())
		assert.NotNil(t, conn.Ping(t.Context()))
	})
}

func TestContextEndClosesConn(t *testing.T) {
	t.Parallel()
	t.Run("read", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			readErr := make(chan error, 1)
			server := newServer(t, func(ctx context.Context, conn *websocket.Conn) {
				readErr <- readUntilError(ctx, conn)
			})
			conn := dial(t, server, nil)
			ctx, cancel := context.WithCancel(t.Context())
			go func() {
				synctest.Wait()
				cancel()
			}()
			_, _, err := conn.Read(ctx)
			assert.ErrorIs(t, err, context.Canceled)
			assert.ErrorIs(t, conn.Write(t.Context(), websocket.MessageText, nil), net.ErrClosed)
			assert.Equal(t, websocket.CloseStatus(<-readErr), -1)
		})
	})
	t.Run("write", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			server := newServer(t, func(ctx context.Context, conn *websocket.Conn) {
				<-release
				_ = readUntilError(ctx, conn)
			})
			defer close(release)
			conn := dial(t, server, nil)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 1<<16))
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			_, _, err = conn.Read(t.Context())
			assert.ErrorIs(t, err, net.ErrClosed)
		})
	})
}

func TestCloseStatus(t *testing.T) {
	t.Parallel()
	assert.Equal(t, websocket.CloseStatus(nil), -1)
	assert.Equal(t, websocket.CloseStatus(errors.New("x")), -1)
	closeErr := websocket.CloseError{Code: websocket.StatusGoingAway, Reason: "bye"}
	assert.Equal(t, websocket.CloseStatus(closeErr), websocket.StatusGoingAway)
	assert.Equal(t, closeErr.Error(), `status = 1001 and reason = "bye"`)
}

func newServer(t *testing.T, handle func(context.Context, *websocket.Conn)) *memhttp.Server {
	t.Helper()
	return memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(response, request, &websocket.AcceptOptions{Subprotocols: []string{testSubprotocol}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		handle(context.WithoutCancel(request.Context()), conn)
	}))
}

func dial(t *testing.T, server *memhttp.Server, opts *websocket.DialOptions) *websocket.Conn {
	t.Helper()
	if opts == nil {
		opts = &websocket.DialOptions{}
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Transport: server.TransportHTTP1()}
	}
	if opts.Subprotocols == nil {
		opts.Subprotocols = []string{testSubprotocol}
	}
	conn, response, err := websocket.Dial(t.Context(), server.URL(), opts) //nolint:bodyclose // no body on success
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	assert.Nil(t, response.Body)
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func echo(ctx context.Context, conn *websocket.Conn) {
	for {
		messageType, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if err := conn.Write(ctx, messageType, data); err != nil {
			return
		}
	}
}

func readUntilError(ctx context.Context, conn *websocket.Conn) error {
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return err
		}
	}
}

// rawDial makes the handshake by hand and returns the raw connection, for
// tests that send arbitrary frames. Extra bytes go out with the handshake.
func rawDial(t *testing.T, server *memhttp.Server, extra []byte) (net.Conn, *bufio.Reader) {
	t.Helper()
	netConn, err := server.TransportHTTP1().DialContext(t.Context(), "tcp", "memhttp")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = netConn.Close() })
	handshake := "GET / HTTP/1.1\r\nHost: " + strings.TrimPrefix(server.URL(), "http://") + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + testKey + "\r\nSec-WebSocket-Protocol: " + testSubprotocol + "\r\n\r\n"
	if _, err := netConn.Write(append([]byte(handshake), extra...)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	reader := bufio.NewReader(netConn)
	response, err := http.ReadResponse(reader, nil) //nolint:bodyclose // a 101 has no body
	if err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	assert.Equal(t, response.StatusCode, http.StatusSwitchingProtocols)
	assert.Equal(t, response.Header.Get("Sec-WebSocket-Accept"), testAccept)
	return netConn, reader
}

// rawUpgradeHandler answers the handshake by hand, letting mutate change the
// response headers, then hands the raw connection to serve.
func rawUpgradeHandler(mutate func(http.Header), serve func(net.Conn, *bufio.Reader)) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		netConn, readWriter, err := http.NewResponseController(response).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = netConn.Close() }()
		hash := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		header := make(http.Header)
		header.Set("Upgrade", "websocket")
		header.Set("Connection", "Upgrade")
		header.Set("Sec-WebSocket-Accept", base64.StdEncoding.EncodeToString(hash[:]))
		header.Set("Sec-WebSocket-Protocol", request.Header.Get("Sec-WebSocket-Protocol"))
		mutate(header)
		var handshake bytes.Buffer
		handshake.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
		_ = header.Write(&handshake)
		handshake.WriteString("\r\n")
		if _, err := netConn.Write(handshake.Bytes()); err != nil {
			return
		}
		serve(netConn, readWriter.Reader)
	})
}

// appendFrame appends a frame with a fixed mask key when masked.
func appendFrame(dst []byte, first byte, payload []byte, masked bool) []byte {
	var maskBit byte
	if masked {
		maskBit = 0x80
	}
	switch length := len(payload); {
	case length <= 125:
		dst = append(dst, first, maskBit|byte(length))
	case length <= 65535:
		dst = append(dst, first, maskBit|126)
		dst = binary.BigEndian.AppendUint16(dst, uint16(length))
	default:
		dst = append(dst, first, maskBit|127)
		dst = binary.BigEndian.AppendUint64(dst, uint64(length))
	}
	if !masked {
		return append(dst, payload...)
	}
	key := [4]byte{0x12, 0x34, 0x56, 0x78}
	dst = append(dst, key[:]...)
	for i, char := range payload {
		dst = append(dst, char^key[i%4])
	}
	return dst
}

func closePayload(code websocket.StatusCode, reason string) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(code)), reason...)
}

// readRawFrame reads a frame of up to 64 KiB, unmasking it if needed.
func readRawFrame(t *testing.T, reader *bufio.Reader) (byte, []byte) {
	t.Helper()
	var head [2]byte
	if _, err := io.ReadFull(reader, head[:]); err != nil {
		t.Errorf("read frame: %v", err)
		return 0, nil
	}
	length := int(head[1] & 0x7f)
	if length == 126 {
		var extended [2]byte
		_, _ = io.ReadFull(reader, extended[:])
		length = int(binary.BigEndian.Uint16(extended[:]))
	}
	var key [4]byte
	if head[1]&0x80 != 0 {
		_, _ = io.ReadFull(reader, key[:])
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Errorf("read payload: %v", err)
		return 0, nil
	}
	for i := range payload {
		payload[i] ^= key[i%4]
	}
	return head[0], payload
}

// readCloseFrame skips frames until a Close frame and returns its code and
// reason.
func readCloseFrame(t *testing.T, reader *bufio.Reader) (websocket.StatusCode, string) {
	t.Helper()
	for {
		first, payload := readRawFrame(t, reader)
		if first == 0 {
			return -1, ""
		}
		if first&0x0f != opClose {
			continue
		}
		if len(payload) < 2 {
			return websocket.StatusNoStatusRcvd, ""
		}
		return websocket.StatusCode(binary.BigEndian.Uint16(payload)), string(payload[2:])
	}
}
