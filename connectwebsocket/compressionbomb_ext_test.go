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
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

const (
	bombReadMaxBytes = 64 * 1024       // what the server will accept
	bombCompressed   = 8 * 1024 * 1024 // what the attacker sends
	bombWireCeiling  = 1024 * 1024     // generous: unbounded reading would blow past this
)

// countingListener reports how many bytes reached the server, which is the
// only way to tell a bounded read from one that ran to completion.
type countingListener struct {
	net.Listener

	read atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, read: &l.read}, nil
}

type countingConn struct {
	net.Conn

	read *atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// zeroOutputDeflate builds a DEFLATE stream of empty sync-flush blocks. Each
// 4-byte marker inflates to nothing, so the compressed size grows without the
// inflated size ever moving — the shape that would defeat a limit applied only
// after inflation.
func zeroOutputDeflate(size int) []byte {
	payload := make([]byte, 0, size)
	for len(payload) < size {
		payload = append(payload, 0x00, 0x00, 0xFF, 0xFF)
	}
	// permessage-deflate strips the trailing marker; the receiver appends it.
	return payload[:len(payload)-4]
}

// writeClientFrame writes one masked client frame with FIN set. compressed
// sets RSV1, which marks the payload as permessage-deflate compressed.
func writeClientFrame(conn net.Conn, payload []byte, compressed bool) error {
	first := byte(0x80 | 0x02) // FIN | binary
	if compressed {
		first |= 0x40 // RSV1
	}
	header := []byte{first}
	switch n := len(payload); {
	case n < 126:
		header = append(header, byte(0x80|n))
	case n < 65536:
		header = append(header, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(header[len(header)-2:], uint16(n))
	default:
		header = append(header, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[len(header)-8:], uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header = append(header, mask[:]...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(masked)
	return err
}

// rawHandshake performs a WebSocket upgrade by hand, so the test controls the
// extension negotiation that coder's client would otherwise own.
func rawHandshake(tb testing.TB, conn net.Conn, addr, procedure string) string {
	tb.Helper()
	var keyBytes [16]byte
	_, err := rand.Read(keyBytes[:])
	assert.Nil(tb, err)
	request := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n"+
			"Sec-WebSocket-Protocol: connect.v2\r\n"+
			"Sec-WebSocket-Extensions: permessage-deflate; client_no_context_takeover; server_no_context_takeover\r\n\r\n",
		procedure, addr, base64.StdEncoding.EncodeToString(keyBytes[:]),
	)
	_, err = conn.Write([]byte(request))
	assert.Nil(tb, err)

	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	assert.Nil(tb, err)
	tb.Cleanup(func() {
		if res.Body != nil {
			_ = res.Body.Close()
		}
	})
	assert.Equal(tb, res.StatusCode, http.StatusSwitchingProtocols)
	// Returned rather than asserted: a caller testing compression needs
	// permessage-deflate, and one testing its absence needs it gone.
	return res.Header.Get("Sec-WebSocket-Extensions")
}

// TestCompressionBombIsBoundedOnTheWire pins a property the transport's safety
// depends on but does not itself implement: the read limit bounds compressed
// input, not only the inflated message. A stream of zero-output DEFLATE blocks
// never trips an inflated-size limit, so if the compressed side were unbounded
// a peer could make the server read forever from one frame.
//
// This guards a dependency, so it is the test that should fail first if a
// WebSocket library upgrade changes the behavior.
func TestCompressionBombIsBoundedOnTheWire(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	options := []connectwebsocket.ServerOption{connectwebsocket.WithReadMaxBytes(bombReadMaxBytes)}
	connecthttp.Mount(
		connectwebsocket.Mux(mux, server, options...),
		server,
	)

	httpServer := httptest.NewUnstartedServer(mux)
	listener := &countingListener{Listener: httpServer.Listener}
	httpServer.Listener = listener
	httpServer.Start()
	t.Cleanup(httpServer.Close)

	addr := httpServer.Listener.Addr().String()
	dialer := &net.Dialer{}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// Without permessage-deflate the payload below is not treated as compressed
	// and the test would prove nothing.
	assert.True(t, strings.Contains(rawHandshake(t, conn, addr, pingProcedure), "permessage-deflate"))
	handshakeBytes := listener.read.Load()

	// A partial write is the expected outcome: the server stops reading and
	// tears the connection down well before 8MiB has been sent.
	assert.Nil(t, conn.SetWriteDeadline(time.Now().Add(20*time.Second)))
	_ = writeClientFrame(conn, zeroOutputDeflate(bombCompressed), true)

	// Drain until the server closes, so the measurement covers everything it
	// was willing to read.
	assert.Nil(t, conn.SetReadDeadline(time.Now().Add(20*time.Second)))
	_, _ = io.Copy(io.Discard, conn)

	bombBytes := listener.read.Load() - handshakeBytes
	t.Logf("server consumed %d KiB of an %d KiB bomb (limit %d KiB)",
		bombBytes/1024, bombCompressed/1024, bombReadMaxBytes/1024)
	assert.True(t, bombBytes < bombWireCeiling)
}

// hostileServer completes a WebSocket handshake by hand and then sends the
// client a compression bomb. coder's own server would compress for us, so the
// frame has to be built directly.
func hostileServer(tb testing.TB, read *atomic.Int64) string {
	tb.Helper()
	listener, err := (&net.ListenConfig{}).Listen(tb.Context(), "tcp", "127.0.0.1:0")
	assert.Nil(tb, err)
	tb.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		request, readErr := http.ReadRequest(reader)
		if readErr != nil {
			return
		}
		sum := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + websocketGUID))
		// Echo whatever the client offered: it verifies the server agreed to the
		// token it asked for, which follows its codec.
		_, _ = fmt.Fprintf(conn,
			"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
				"Sec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: %s\r\n"+
				"Sec-WebSocket-Extensions: permessage-deflate; client_no_context_takeover; server_no_context_takeover\r\n\r\n",
			base64.StdEncoding.EncodeToString(sum[:]),
			request.Header.Get("Sec-WebSocket-Protocol"),
		)
		// Server frames are unmasked; FIN | RSV1 | binary.
		payload := zeroOutputDeflate(bombCompressed)
		header := []byte{0x80 | 0x40 | 0x02, 127, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint64(header[2:10], uint64(len(payload)))
		_, _ = conn.Write(header)
		_, _ = conn.Write(payload)
		// Hold the connection open so any draining by the client is visible.
		time.Sleep(2 * time.Second)
	}()
	return "http://" + listener.Addr().String()
}

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// TestCompressionBombFromServerIsBoundedOnTheWire is the client-side mirror:
// a hostile server must not be able to make the client drain an oversized
// frame while closing.
func TestCompressionBombFromServerIsBoundedOnTheWire(t *testing.T) {
	t.Parallel()
	var clientRead atomic.Int64
	url := hostileServer(t, &clientRead)

	httpClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &countingConn{Conn: conn, read: &clientRead}, nil
		},
	}}
	transport, err := connectwebsocket.NewTransport(
		url,
		connectwebsocket.WithHTTPClient(httpClient),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
		connectwebsocket.WithReadMaxBytes(bombReadMaxBytes),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	_ = stream.Send(&pingv1.CumSumRequest{Number: 1})
	_, receiveErr := stream.Receive()
	assert.NotNil(t, receiveErr)
	assert.Equal(t, connect.CodeOf(receiveErr), connect.CodeResourceExhausted)
	_ = stream.Close()

	consumed := clientRead.Load()
	t.Logf("client consumed %d KiB of an %d KiB bomb (limit %d KiB)",
		consumed/1024, bombCompressed/1024, bombReadMaxBytes/1024)
	assert.True(t, consumed < bombWireCeiling)
}
