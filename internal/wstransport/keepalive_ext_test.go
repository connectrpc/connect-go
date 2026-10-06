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

package wstransport_test

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/wstransport"
)

const (
	opcodePing = 0x09
	opcodePong = 0x0A

	// keepAliveInterval is short, for tests that wait for a hang-up.
	keepAliveInterval = 20 * time.Millisecond
	// sparedInterval leaves room for scheduling delays under -race, for tests
	// that a peer must survive: a slow Pong there reads as a dead peer.
	sparedInterval = 100 * time.Millisecond

	// noCloseFrame is what watchFrames reports when the connection ended, or
	// its deadline passed, without a close frame.
	noCloseFrame = -1
)

// nextFrame reads one frame in either direction, unmasking it if needed. The
// library answers control frames itself, so reading the bytes is the only way
// to see a Ping.
func nextFrame(reader io.Reader) (opcode byte, payload []byte, err error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, nil, err
	}
	opcode = header[0] & 0x0F
	masked := header[1]&0x80 != 0
	size := uint64(header[1] & 0x7F)
	switch size {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(reader, extended); err != nil {
			return 0, nil, err
		}
		size = uint64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(reader, extended); err != nil {
			return 0, nil, err
		}
		size = binary.BigEndian.Uint64(extended)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for index := range payload {
			payload[index] ^= mask[index%4]
		}
	}
	return opcode, payload, nil
}

// watchFrames reads until a close frame arrives or reading fails, counting
// Pings and passing each to answer when it is non-nil. It reports the close
// code, or noCloseFrame. The caller bounds it with a deadline on the conn.
func watchFrames(reader io.Reader, answer func(payload []byte)) (pings, closeCode int) {
	for {
		opcode, payload, err := nextFrame(reader)
		if err != nil {
			return pings, noCloseFrame
		}
		switch opcode {
		case opcodePing:
			pings++
			if answer != nil {
				answer(payload)
			}
		case opcodeClose:
			if len(payload) < 2 {
				return pings, noCloseFrame
			}
			return pings, int(binary.BigEndian.Uint16(payload))
		}
	}
}

// newKeepAliveServer serves the ping service with the given server options.
func newKeepAliveServer(tb testing.TB, options ...wstransport.ServerOption) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	mountBoth(mux, server, options...)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// openRawCumSum upgrades by hand and opens a CumSum stream, leaving the
// handler blocked in Receive so the server sends nothing of its own.
func openRawCumSum(tb testing.TB, httpServer *httptest.Server) net.Conn {
	tb.Helper()
	addr := httpServer.Listener.Addr().String()
	conn, err := (&net.Dialer{}).DialContext(tb.Context(), "tcp", addr)
	assert.Nil(tb, err)
	tb.Cleanup(func() { _ = conn.Close() })
	_ = rawHandshake(tb, conn, addr, pingProcedure)
	assert.Nil(tb, writeClientFrame(conn, openMessage(), false, true))
	return conn
}

// A client that goes silent is sent 1011 once two intervals pass with nothing
// from it, the Pings in between notwithstanding.
func TestServerHangsUpOnSilentClient(t *testing.T) {
	t.Parallel()
	httpServer := newKeepAliveServer(t, wstransport.WithKeepAlive(keepAliveInterval))
	conn := openRawCumSum(t, httpServer)

	assert.Nil(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	pings, closeCode := watchFrames(conn, nil)
	assert.True(t, pings >= 1)
	assert.Equal(t, closeCode, 1011)
}

// A Pong is a sign of life on its own: a client that sends no messages but
// answers every Ping outlives many timeouts.
func TestServerSparesClientThatAnswersPings(t *testing.T) {
	t.Parallel()
	httpServer := newKeepAliveServer(t, wstransport.WithKeepAlive(sparedInterval))
	conn := openRawCumSum(t, httpServer)

	assert.Nil(t, conn.SetReadDeadline(time.Now().Add(6*sparedInterval)))
	pings, closeCode := watchFrames(conn, func(payload []byte) {
		_ = writeClientFragment(conn, payload, true, opcodePong)
	})
	assert.Equal(t, closeCode, noCloseFrame)
	assert.True(t, pings >= 3)
}

func TestZeroKeepAliveSendsNoPings(t *testing.T) {
	t.Parallel()
	httpServer := newKeepAliveServer(t, wstransport.WithKeepAlive(0))
	conn := openRawCumSum(t, httpServer)

	assert.Nil(t, conn.SetReadDeadline(time.Now().Add(10*keepAliveInterval)))
	_, err := conn.Read(make([]byte, 1))
	assert.True(t, errors.Is(err, os.ErrDeadlineExceeded))
}

// acceptRawUpgrade completes one WebSocket handshake by hand, so the test can
// see the client's frames before any library interprets them. The returned
// reader holds whatever the client sent after the request.
func acceptRawUpgrade(tb testing.TB, listener net.Listener) *bufio.Reader {
	tb.Helper()
	conn, err := listener.Accept()
	assert.Nil(tb, err)
	tb.Cleanup(func() { _ = conn.Close() })
	assert.Nil(tb, conn.SetDeadline(time.Now().Add(10*time.Second)))
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	assert.Nil(tb, err)
	digest := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	_, err = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(digest[:]) + "\r\n" +
		"Sec-WebSocket-Protocol: connectrpc.1+proto\r\n\r\n"))
	assert.Nil(tb, err)
	return reader
}

// silentServer accepts one upgrade and then never writes, so a client's
// keep-alive has nothing to hear. It returns the base URL and the reader for
// what the client sends.
func silentServer(tb testing.TB) (string, <-chan *bufio.Reader) {
	tb.Helper()
	listener, err := (&net.ListenConfig{}).Listen(tb.Context(), "tcp", "127.0.0.1:0")
	assert.Nil(tb, err)
	tb.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan *bufio.Reader, 1)
	go func() { accepted <- acceptRawUpgrade(tb, listener) }()
	return "http://" + listener.Addr().String(), accepted
}

// The client pings too, and hangs up on a silent server with 3111, the
// client's counterpart of 1011 (§13). The RPC fails as Unavailable, which
// invites the retry a dead connection deserves.
func TestClientHangsUpOnSilentServer(t *testing.T) {
	t.Parallel()
	baseURL, accepted := silentServer(t)
	transport, err := wstransport.NewTransport(baseURL, wstransport.WithKeepAlive(keepAliveInterval))
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = stream.Close() })
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))

	pings, closeCode := watchFrames(<-accepted, nil)
	assert.True(t, pings >= 1)
	assert.Equal(t, closeCode, 3111)
	_, err = stream.Receive()
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable)
	assert.True(t, strings.Contains(err.Error(), "keep-alive timeout"))
}

// Pings interleave with the stream's own messages without disturbing them,
// and a stream idle for many timeouts carries on: each side's reader answers
// Pings whether or not its caller is receiving.
func TestKeepAliveLeavesIdleStreamIntact(t *testing.T) {
	t.Parallel()
	httpServer := newKeepAliveServer(t, wstransport.WithKeepAlive(sparedInterval))
	transport, err := wstransport.NewTransport(httpServer.URL, wstransport.WithKeepAlive(sparedInterval))
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = stream.Close() })

	for _, number := range []int64{1, 2, 3} {
		assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: number}))
		response, err := stream.Receive()
		assert.Nil(t, err)
		assert.True(t, response.GetSum() > 0)
		time.Sleep(3 * sparedInterval)
	}
	assert.Nil(t, stream.CloseSend())
	_, err = stream.Receive()
	assert.True(t, errors.Is(err, io.EOF))
}

// The cost of keeping backpressure: a client that leaves a message untaken
// stops reading the socket, so it stops answering Pings too, and the server
// hangs up once the timeout passes. Python's websockets makes the same call.
func TestServerHangsUpOnClientThatStopsReceiving(t *testing.T) {
	t.Parallel()
	httpServer := newKeepAliveServer(t, wstransport.WithKeepAlive(keepAliveInterval))
	transport, err := wstransport.NewTransport(httpServer.URL, wstransport.WithKeepAlive(0))
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = stream.Close() })

	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	time.Sleep(10 * keepAliveInterval)
	for range 3 {
		if _, err = stream.Receive(); err != nil {
			break
		}
	}
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable)
}

// The public option reaches both halves: Mount's server and NewTransport's
// client each ping at the interval it names.
func TestConnectHTTPKeepAliveReachesBothSides(t *testing.T) {
	t.Parallel()
	keepAlive := connecthttp.WithWebSocketKeepAlive(keepAliveInterval)

	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, keepAlive)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	conn := openRawCumSum(t, httpServer)
	assert.Nil(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	pings, _ := watchFrames(conn, nil)
	assert.True(t, pings >= 1)

	baseURL, accepted := silentServer(t)
	transport := connecthttp.NewTransport(
		http.DefaultClient,
		baseURL,
		connecthttp.WithWebSocket(connecthttp.SelectStreaming),
		keepAlive,
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = stream.Close() })
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	pings, _ = watchFrames(<-accepted, nil)
	assert.True(t, pings >= 1)
}
