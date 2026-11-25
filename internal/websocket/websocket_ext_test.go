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
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
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

// httpClientFunc adapts a function to [websocket.HTTPClient].
type httpClientFunc func(*http.Request) (*http.Response, error)

func (f httpClientFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestAccept(t *testing.T) {
	t.Parallel()
	server := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(response, request, websocket.Options{Subprotocol: "b"})
		if err == nil {
			_ = conn.Close()
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
		{name: "subprotocol", header: map[string]string{"Sec-WebSocket-Protocol": "x, a, b"}, wantStatus: http.StatusSwitchingProtocols, wantProtocol: "b"},
		{name: "subprotocol_case", header: map[string]string{"Sec-WebSocket-Protocol": "x, B"}, wantStatus: http.StatusSwitchingProtocols},
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
	t.Run("subprotocol_refused", func(t *testing.T) {
		t.Parallel()
		server := newServer(t, echo)
		request := newRequest(t, server.URL())
		_, response, err := websocket.Dial(&http.Client{Transport: server.TransportHTTP1()}, request, websocket.Options{Subprotocol: "other"}) //nolint:bodyclose // Dial set it to http.NoBody
		assert.NotNil(t, err)
		assert.Equal(t, response.StatusCode, http.StatusSwitchingProtocols)
		assert.Equal(t, response.Body, io.ReadCloser(http.NoBody))
	})
	t.Run("http_error", func(t *testing.T) {
		t.Parallel()
		server := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			http.Error(response, "nope", http.StatusTeapot)
		}))
		_, response, err := websocket.Dial(&http.Client{Transport: server.TransportHTTP1()}, newRequest(t, server.URL()), websocket.Options{})
		assert.NotNil(t, err)
		assert.Equal(t, response.StatusCode, http.StatusTeapot)
		body, readErr := io.ReadAll(response.Body)
		assert.Nil(t, readErr)
		assert.Equal(t, string(body), "nope\n")
		assert.Nil(t, response.Body.Close())
	})
	t.Run("invalid_request", func(t *testing.T) {
		t.Parallel()
		requests := map[string]*http.Request{
			"ws_scheme": newRequest(t, "ws://example.com"),
			"post":      newRequest(t, "http://example.com"),
		}
		requests["post"].Method = http.MethodPost
		for name, request := range requests {
			_, response, err := websocket.Dial(http.DefaultClient, request, websocket.Options{}) //nolint:bodyclose // no response
			assert.NotNil(t, err, assert.Sprintf("%s", name))
			assert.Nil(t, response)
		}
	})
	t.Run("broken_http_client", func(t *testing.T) {
		t.Parallel()
		// An HTTPClient other than *http.Client may return no response, or a
		// 101 without a body.
		noResponse := httpClientFunc(func(*http.Request) (*http.Response, error) {
			return nil, nil //nolint:nilnil // a broken HTTPClient
		})
		_, response, err := websocket.Dial(noResponse, newRequest(t, "http://example.com"), websocket.Options{}) //nolint:bodyclose // no response
		assert.NotNil(t, err)
		assert.Nil(t, response)
		noBody := httpClientFunc(func(request *http.Request) (*http.Response, error) {
			hash := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			header := http.Header{
				"Upgrade":              {"websocket"},
				"Connection":           {"Upgrade"},
				"Sec-Websocket-Accept": {base64.StdEncoding.EncodeToString(hash[:])},
			}
			return &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: header}, nil
		})
		_, response, err = websocket.Dial(noBody, newRequest(t, "http://example.com"), websocket.Options{}) //nolint:bodyclose // Dial set it to http.NoBody
		assert.NotNil(t, err)
		assert.Equal(t, response.StatusCode, http.StatusSwitchingProtocols)
	})
	tests := []struct {
		name    string
		mutate  func(http.Header)
		wantErr bool
	}{
		{name: "valid", mutate: func(http.Header) {}},
		{name: "no_subprotocol", mutate: func(header http.Header) { header.Del("Sec-WebSocket-Protocol") }, wantErr: true},
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
			conn, response, err := websocket.Dial(&http.Client{Transport: server.TransportHTTP1()}, newRequest(t, server.URL()), websocket.Options{Subprotocol: testSubprotocol}) //nolint:bodyclose // Dial set it to http.NoBody
			assert.Equal(t, response.StatusCode, http.StatusSwitchingProtocols)
			assert.Equal(t, response.Body, io.ReadCloser(http.NoBody))
			if test.wantErr {
				assert.NotNil(t, err)
				return
			}
			assert.Nil(t, err)
			assert.Nil(t, conn.Close())
		})
	}
}

func TestDialTimeoutBoundsHandshake(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		server := newServer(t, echo)
		conn := dial(t, server, &http.Client{Transport: server.TransportHTTP1(), Timeout: time.Second})
		time.Sleep(2 * time.Second)
		assert.Nil(t, conn.WriteMessage(websocket.MessageText, frameOf("late")))
		var dst bytes.Buffer
		_, err := conn.ReadMessage(&dst, -1)
		assert.Nil(t, err)
		assert.Equal(t, dst.String(), "late")
	})
}

func TestMessageSizes(t *testing.T) {
	t.Parallel()
	server := newServer(t, echo)
	conn := dial(t, server, nil)
	var dst bytes.Buffer
	for _, size := range []int{0, 1, 125, 126, 127, 65535, 65536, 70000} {
		for _, messageType := range []websocket.MessageType{websocket.MessageText, websocket.MessageBinary} {
			data := strings.Repeat("é", size/2)
			if size%2 == 1 {
				data += "x"
			}
			assert.Nil(t, conn.WriteMessage(messageType, frameOf(data)))
			dst.Reset()
			gotType, err := conn.ReadMessage(&dst, -1)
			assert.Nil(t, err)
			assert.Equal(t, gotType, messageType)
			assert.Equal(t, dst.Len(), size)
			assert.True(t, dst.String() == data, assert.Sprintf("size %d", size))
		}
	}
}

func TestReadMessageAppends(t *testing.T) {
	t.Parallel()
	server := newServer(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.MessageText, frameOf("skipped"))
		_ = conn.WriteMessage(websocket.MessageBinary, frameOf("first"))
		_ = conn.WriteMessage(websocket.MessageBinary, frameOf("second"))
		_ = readUntilError(conn)
	})
	conn := dial(t, server, nil)
	// A nil dst discards the message but still reports its type.
	messageType, err := conn.ReadMessage(nil, -1)
	assert.Nil(t, err)
	assert.Equal(t, messageType, websocket.MessageText)
	dst := bytes.NewBufferString("kept:")
	_, err = conn.ReadMessage(dst, 5)
	assert.Nil(t, err)
	// The limit doesn't count bytes already in dst.
	_, err = conn.ReadMessage(dst, 6)
	assert.Nil(t, err)
	assert.Equal(t, dst.String(), "kept:firstsecond")
}

func TestWriteMessageErrors(t *testing.T) {
	t.Parallel()
	server := newServer(t, echo)
	conn := dial(t, server, nil)
	assert.NotNil(t, conn.WriteMessage(websocket.MessageBinary, make([]byte, websocket.MaxHeaderSize-1)))
	assert.NotNil(t, conn.WriteMessage(websocket.MessageType(3), frameOf("")))
	assert.Nil(t, conn.WriteMessage(websocket.MessageBinary, frameOf("")))
}

func TestFragmentedMessage(t *testing.T) {
	t.Parallel()
	server := newServer(t, echo)
	netConn, reader := rawDial(t, server, nil)
	var frames []byte
	// The é is split across fragments, with control frames between them.
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
		wantErr    error
	}{
		{name: "rsv1", frames: [][]byte{textFrame(finBit|0x40|opText, "a")}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "opcode_3", frames: [][]byte{textFrame(finBit|0x3, "a")}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "opcode_11", frames: [][]byte{textFrame(finBit|0xb, "")}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "unmasked", frames: [][]byte{appendFrame(nil, finBit|opText, []byte("a"), false)}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "long_ping", frames: [][]byte{textFrame(finBit|opPing, strings.Repeat("p", 126))}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "fragmented_ping", frames: [][]byte{textFrame(opPing, "p")}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "lone_continuation", frames: [][]byte{textFrame(finBit|opCont, "a")}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "interleaved_message", frames: [][]byte{textFrame(opText, "a"), textFrame(finBit|opText, "b")}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "length_msb", frames: [][]byte{{finBit | opBinary, 0xff, 0x80, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0}}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "close_one_byte", frames: [][]byte{closeFrame([]byte{3})}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "close_999", frames: [][]byte{closeFrame(closePayload(999, ""))}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "close_1004", frames: [][]byte{closeFrame(closePayload(1004, ""))}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "close_1005", frames: [][]byte{closeFrame(closePayload(1005, ""))}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "close_1006", frames: [][]byte{closeFrame(closePayload(1006, ""))}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "close_1015", frames: [][]byte{closeFrame(closePayload(1015, ""))}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "close_2999", frames: [][]byte{closeFrame(closePayload(2999, ""))}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "close_5000", frames: [][]byte{closeFrame(closePayload(5000, ""))}, wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "close_reason_utf8", frames: [][]byte{closeFrame(closePayload(1000, "\xff"))}, wantStatus: websocket.StatusInvalidPayloadData, wantErr: websocket.ErrInvalidUTF8},
		{name: "text_utf8", frames: [][]byte{textFrame(finBit|opText, "a\xffb")}, wantStatus: websocket.StatusInvalidPayloadData, wantErr: websocket.ErrInvalidUTF8},
		{name: "text_ends_in_rune", frames: [][]byte{textFrame(opText, "a"), textFrame(finBit|opCont, "\xc3")}, wantStatus: websocket.StatusInvalidPayloadData, wantErr: websocket.ErrInvalidUTF8},
		{name: "too_big", frames: [][]byte{textFrame(finBit|opBinary, strings.Repeat("x", readLimit+1))}, wantStatus: websocket.StatusMessageTooBig, wantErr: websocket.ErrMessageTooBig},
		{name: "too_big_fragments", frames: [][]byte{textFrame(opBinary, "0123456789"), textFrame(finBit|opCont, "0123456789")}, wantStatus: websocket.StatusMessageTooBig, wantErr: websocket.ErrMessageTooBig},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			readErrs := make(chan [2]error, 1)
			server := newServer(t, func(conn *websocket.Conn) {
				var dst bytes.Buffer
				var errs [2]error
				for errs[0] == nil {
					_, errs[0] = conn.ReadMessage(&dst, readLimit)
				}
				// The call after the error drains, then returns it again.
				_, errs[1] = conn.ReadMessage(&dst, readLimit)
				readErrs <- errs
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
			// Answer the Close, which ends the drain. A server that already
			// received a Close has closed the connection.
			_, _ = netConn.Write(appendFrame(nil, finBit|opClose, closePayload(code, ""), true))
			_, err := io.Copy(io.Discard, reader)
			assert.Nil(t, err)
			errs := <-readErrs
			assert.ErrorIs(t, errs[0], test.wantErr)
			assert.ErrorIs(t, errs[1], errs[0])
			// The code this side sent isn't the peer's Close.
			_, isCloseErr := errors.AsType[*websocket.CloseError](errs[0])
			assert.False(t, isCloseErr)
		})
	}
}

func TestClientProtocolErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		frame      []byte
		wantStatus websocket.StatusCode
		wantErr    error
	}{
		{name: "masked", frame: appendFrame(nil, finBit|opText, []byte("a"), true), wantStatus: websocket.StatusProtocolError, wantErr: websocket.ErrProtocol},
		{name: "text_utf8", frame: appendFrame(nil, finBit|opText, []byte("M\xff"), false), wantStatus: websocket.StatusInvalidPayloadData, wantErr: websocket.ErrInvalidUTF8},
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
			_, err := conn.ReadMessage(&bytes.Buffer{}, -1)
			assert.ErrorIs(t, err, test.wantErr)
			assert.Equal(t, <-gotStatus, test.wantStatus)
			// The server hangs up, which ends the drain.
			_, again := conn.ReadMessage(&bytes.Buffer{}, -1)
			assert.ErrorIs(t, again, err)
		})
	}
}

func TestReceiveClose(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		payload  []byte
		want     websocket.CloseError
		wantEcho []byte
	}{
		{name: "normal", payload: closePayload(1000, "bye"), want: websocket.CloseError{Code: 1000, Reason: "bye"}, wantEcho: closePayload(1000, "")},
		{name: "no_status", want: websocket.CloseError{Code: websocket.StatusNoStatusReceived}},
		{name: "service_restart", payload: closePayload(1012, ""), want: websocket.CloseError{Code: 1012}, wantEcho: closePayload(1012, "")},
		{name: "application", payload: closePayload(3000, "é"), want: websocket.CloseError{Code: 3000, Reason: "é"}, wantEcho: closePayload(3000, "")},
		{name: "private", payload: closePayload(4999, ""), want: websocket.CloseError{Code: 4999}, wantEcho: closePayload(4999, "")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			readErr := make(chan error, 1)
			server := newServer(t, func(conn *websocket.Conn) {
				readErr <- readUntilError(conn)
			})
			netConn, reader := rawDial(t, server, nil)
			go func() { _, _ = netConn.Write(appendFrame(nil, finBit|opClose, test.payload, true)) }()
			first, echoed := readRawFrame(t, reader)
			assert.Equal(t, first, finBit|opClose)
			assert.Equal(t, len(echoed), len(test.wantEcho))
			assert.True(t, bytes.Equal(echoed, test.wantEcho))
			closeErr, ok := errors.AsType[*websocket.CloseError](<-readErr)
			assert.True(t, ok)
			assert.Equal(t, *closeErr, test.want)
		})
	}
}

func TestCloseHandshake(t *testing.T) {
	t.Parallel()
	t.Run("client_closes", func(t *testing.T) {
		t.Parallel()
		readErr := make(chan error, 1)
		server := newServer(t, func(conn *websocket.Conn) {
			readErr <- readUntilError(conn)
		})
		conn := dial(t, server, nil)
		assert.Nil(t, conn.WriteClose(websocket.StatusNormalClosure, "bye"))
		_, err := conn.ReadMessage(&bytes.Buffer{}, -1)
		assert.Equal(t, err, error(&websocket.CloseError{Code: websocket.StatusNormalClosure}))
		assert.Equal(t, <-readErr, error(&websocket.CloseError{Code: websocket.StatusNormalClosure, Reason: "bye"}))
		// The handshake is over and the transport closed.
		_, again := conn.ReadMessage(&bytes.Buffer{}, -1)
		assert.Equal(t, again, err)
		assert.ErrorIs(t, conn.WriteMessage(websocket.MessageText, frameOf("")), net.ErrClosed)
		assert.ErrorIs(t, conn.Ping(), net.ErrClosed)
		assert.Nil(t, conn.WriteClose(websocket.StatusGoingAway, ""))
		assert.Nil(t, conn.Close())
		assert.Nil(t, conn.Close())
	})
	t.Run("server_closes", func(t *testing.T) {
		t.Parallel()
		readErr := make(chan error, 1)
		server := newServer(t, func(conn *websocket.Conn) {
			if err := conn.WriteClose(websocket.StatusGoingAway, "bye"); err != nil {
				readErr <- err
				return
			}
			// Data the client sent before it saw the Close is discarded.
			var dst bytes.Buffer
			_, err := conn.ReadMessage(&dst, -1)
			assert.Equal(t, dst.Len(), 0)
			readErr <- err
		})
		conn := dial(t, server, nil)
		writing := make(chan struct{})
		go func() {
			defer close(writing)
			for {
				if err := conn.WriteMessage(websocket.MessageText, frameOf("ignored")); err != nil {
					return
				}
			}
		}()
		_, err := conn.ReadMessage(&bytes.Buffer{}, -1)
		assert.Equal(t, err, error(&websocket.CloseError{Code: websocket.StatusGoingAway, Reason: "bye"}))
		assert.Equal(t, <-readErr, error(&websocket.CloseError{Code: websocket.StatusGoingAway}))
		<-writing
	})
	t.Run("no_status", func(t *testing.T) {
		t.Parallel()
		readErr := make(chan error, 1)
		server := newServer(t, func(conn *websocket.Conn) {
			readErr <- readUntilError(conn)
		})
		conn := dial(t, server, nil)
		assert.NotNil(t, conn.WriteClose(1006, ""))
		assert.Nil(t, conn.WriteClose(websocket.StatusNoStatusReceived, strings.Repeat("ignored", 10)))
		_, err := conn.ReadMessage(&bytes.Buffer{}, -1)
		assert.Equal(t, err, error(&websocket.CloseError{Code: websocket.StatusNoStatusReceived}))
		assert.Equal(t, <-readErr, error(&websocket.CloseError{Code: websocket.StatusNoStatusReceived}))
	})
	t.Run("long_reason", func(t *testing.T) {
		t.Parallel()
		readErr := make(chan error, 1)
		server := newServer(t, func(conn *websocket.Conn) {
			readErr <- readUntilError(conn)
		})
		conn := dial(t, server, nil)
		assert.Nil(t, conn.WriteClose(3000, strings.Repeat("é", 100)))
		_, _ = conn.ReadMessage(nil, -1)
		closeErr, ok := errors.AsType[*websocket.CloseError](<-readErr)
		assert.True(t, ok)
		assert.Equal(t, closeErr.Reason, strings.Repeat("é", 61))
	})
}

func TestReadLimit(t *testing.T) {
	t.Parallel()
	t.Run("too_big", func(t *testing.T) {
		t.Parallel()
		readErrs := make(chan [2]error, 1)
		server := newServer(t, func(conn *websocket.Conn) {
			var errs [2]error
			_, errs[0] = conn.ReadMessage(&bytes.Buffer{}, 4)
			_, errs[1] = conn.ReadMessage(nil, -1)
			readErrs <- errs
		})
		conn := dial(t, server, nil)
		// The server stops reading mid-frame, which blocks this write on the
		// pipe until the server's drain gives up and closes.
		writeErr := make(chan error, 1)
		go func() { writeErr <- conn.WriteMessage(websocket.MessageBinary, frameOf(strings.Repeat("x", 1<<20))) }()
		_, err := conn.ReadMessage(nil, -1)
		assert.Equal(t, err, error(&websocket.CloseError{Code: websocket.StatusMessageTooBig}))
		assert.NotNil(t, <-writeErr)
		errs := <-readErrs
		assert.ErrorIs(t, errs[0], websocket.ErrMessageTooBig)
		assert.ErrorIs(t, errs[1], errs[0])
	})
	t.Run("unlimited", func(t *testing.T) {
		t.Parallel()
		server := newServer(t, echo)
		conn := dial(t, server, nil)
		assert.Nil(t, conn.WriteMessage(websocket.MessageBinary, frameOf(strings.Repeat("x", 1<<20))))
		var dst bytes.Buffer
		_, err := conn.ReadMessage(&dst, -1)
		assert.Nil(t, err)
		assert.Equal(t, dst.Len(), 1<<20)
	})
}

func TestCloseErrorString(t *testing.T) {
	t.Parallel()
	closeErr := &websocket.CloseError{Code: websocket.StatusGoingAway, Reason: "bye"}
	assert.Equal(t, closeErr.Error(), `websocket: peer closed with 1001 "bye"`)
}

func newServer(t *testing.T, handle func(*websocket.Conn)) *memhttp.Server {
	t.Helper()
	return memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(response, request, websocket.Options{Subprotocol: testSubprotocol})
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		handle(conn)
	}))
}

func newRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return request
}

func dial(t *testing.T, server *memhttp.Server, httpClient *http.Client) *websocket.Conn {
	t.Helper()
	if httpClient == nil {
		httpClient = &http.Client{Transport: server.TransportHTTP1()}
	}
	conn, response, err := websocket.Dial(httpClient, newRequest(t, server.URL()), websocket.Options{Subprotocol: testSubprotocol}) //nolint:bodyclose // Dial set it to http.NoBody
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	assert.Equal(t, response.Body, io.ReadCloser(http.NoBody))
	assert.Equal(t, response.Header.Get("Sec-WebSocket-Protocol"), testSubprotocol)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func frameOf(payload string) []byte {
	return append(make([]byte, websocket.MaxHeaderSize, websocket.MaxHeaderSize+len(payload)), payload...)
}

// echo writes each message back until the connection ends. The read buffer
// starts with the header space, which makes the echo copy nothing.
func echo(conn *websocket.Conn) {
	var headerSpace [websocket.MaxHeaderSize]byte
	var frame bytes.Buffer
	for {
		frame.Reset()
		frame.Write(headerSpace[:])
		messageType, err := conn.ReadMessage(&frame, -1)
		if err != nil {
			_, _ = conn.ReadMessage(nil, -1)
			return
		}
		if err := conn.WriteMessage(messageType, frame.Bytes()); err != nil {
			return
		}
	}
}

func readUntilError(conn *websocket.Conn) error {
	var dst bytes.Buffer
	for {
		dst.Reset()
		if _, err := conn.ReadMessage(&dst, -1); err != nil {
			return err
		}
	}
}

// rawDial makes the handshake by hand and returns the raw connection, for
// tests that send arbitrary frames. Extra bytes go out with the handshake.
func rawDial(t *testing.T, server *memhttp.Server, extra []byte) (net.Conn, *bufio.Reader) {
	t.Helper()
	netConn, reader, _ := rawDialHeader(t, server, "", extra)
	return netConn, reader
}

// rawDialHeader is rawDial with extra header lines, each ending in CRLF. It
// also returns the handshake response's header.
func rawDialHeader(t *testing.T, server *memhttp.Server, header string, extra []byte) (net.Conn, *bufio.Reader, http.Header) {
	t.Helper()
	netConn, err := server.TransportHTTP1().DialContext(t.Context(), "tcp", "memhttp")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = netConn.Close() })
	handshake := "GET / HTTP/1.1\r\nHost: " + strings.TrimPrefix(server.URL(), "http://") + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + testKey + "\r\nSec-WebSocket-Protocol: " + testSubprotocol + "\r\n" + header + "\r\n"
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
	return netConn, reader, response.Header
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
			return 0, ""
		}
		if first&0x0f != opClose {
			continue
		}
		if len(payload) < 2 {
			return websocket.StatusNoStatusReceived, ""
		}
		return websocket.StatusCode(binary.BigEndian.Uint16(payload)), string(payload[2:])
	}
}
