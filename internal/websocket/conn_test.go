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

package websocket

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"

	"connectrpc.com/connect/v2/internal/assert"
)

// TestPingCrossesBlockedWrite has the server PING while the client is blocked
// in a large write the server isn't reading yet. net.Pipe has no buffer, which
// would deadlock a reader that answered the PING itself.
func TestPingCrossesBlockedWrite(t *testing.T) {
	t.Parallel()
	client, server, writing := newPipePair(t)
	big := bytes.Repeat([]byte{1}, 1<<20)
	writeErr := make(chan error, 1)
	go func() { writeErr <- client.WriteMessage(MessageBinary, frameOf(big)) }()
	<-writing
	readErr := make(chan error, 1)
	go func() {
		var dst bytes.Buffer
		_, err := client.ReadMessage(&dst, -1)
		readErr <- err
	}()
	assert.Nil(t, server.Ping())
	assert.Nil(t, server.WriteMessage(MessageText, frameOf([]byte("hi"))))
	// The client's reader got past the PING while the write held writeMu.
	assert.Nil(t, <-readErr)
	assert.True(t, client.pongPending.Load())
	var dst bytes.Buffer
	_, err := server.ReadMessage(&dst, -1)
	assert.Nil(t, err)
	assert.Equal(t, dst.Len(), len(big))
	// The writer sends the PONG before it unlocks, and on a pipe that blocks
	// until the server reads it. The server's reader drops it and waits on.
	go func() { _ = client.WriteMessage(MessageText, frameOf([]byte("end"))) }()
	dst.Reset()
	_, err = server.ReadMessage(&dst, -1)
	assert.Nil(t, err)
	assert.Equal(t, dst.String(), "end")
	assert.Nil(t, <-writeErr)
	assert.False(t, client.pongPending.Load())
}

func TestConcurrentPingAndWrite(t *testing.T) {
	t.Parallel()
	client, server, _ := newPipePair(t)
	const writers, perWriter = 4, 200
	var group sync.WaitGroup
	for range writers {
		group.Go(func() {
			for range perWriter {
				assert.Nil(t, server.Ping())
				assert.Nil(t, server.WriteMessage(MessageBinary, frameOf(make([]byte, 300))))
			}
		})
	}
	serverRead := make(chan error, 1)
	go func() {
		for {
			if _, err := server.ReadMessage(nil, -1); err != nil {
				serverRead <- err
				return
			}
		}
	}()
	// Only the server pings, as in connecthttp. On net.Pipe two readers that
	// both answer PINGs can block on each other's PONG until the Conn closes.
	group.Go(func() {
		for range perWriter {
			assert.Nil(t, client.WriteMessage(MessageText, frameOf([]byte("hello"))))
		}
	})
	// The client reads until the end, because a PONG the server never read
	// would block the client's reader on the pipe.
	clientRead := make(chan error, 1)
	received := 0
	go func() {
		var dst bytes.Buffer
		for {
			dst.Reset()
			if _, err := client.ReadMessage(&dst, -1); err != nil {
				clientRead <- err
				return
			}
			assert.Equal(t, dst.Len(), 300)
			received++
		}
	}()
	group.Wait()
	assert.Nil(t, client.WriteClose(StatusNormalClosure, ""))
	assert.Equal(t, <-clientRead, error(&CloseError{Code: StatusNormalClosure}))
	assert.Equal(t, received, writers*perWriter)
	assert.Equal(t, <-serverRead, error(&CloseError{Code: StatusNormalClosure}))
}

//nolint:paralleltest // AllocsPerRun panics in parallel tests
func TestZeroAllocs(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1024)
	for _, client := range []bool{false, true} {
		t.Run(fmt.Sprintf("client_%t", client), func(t *testing.T) {
			// The peer sends a PING before every message, which also
			// measures the PONG reply.
			var stream bytes.Buffer
			peer := newConn(&bufferTransport{buffer: &stream}, nil, !client)
			assert.Nil(t, peer.Ping())
			assert.Nil(t, peer.WriteMessage(MessageText, frameOf(payload)))
			transport := &loopTransport{data: stream.Bytes()}
			conn := newConn(transport, bufio.NewReader(transport), client)
			var dst bytes.Buffer
			read := testing.AllocsPerRun(100, func() {
				dst.Reset()
				_, err := conn.ReadMessage(&dst, -1)
				assert.Nil(t, err)
			})
			assert.Equal(t, read, 0)
			assert.Equal(t, dst.Len(), len(payload))
			frame := frameOf(payload)
			write := testing.AllocsPerRun(100, func() {
				assert.Nil(t, conn.WriteMessage(MessageBinary, frame))
			})
			assert.Equal(t, write, 0)
			ping := testing.AllocsPerRun(100, func() {
				assert.Nil(t, conn.Ping())
			})
			assert.Equal(t, ping, 0)
		})
	}
}

func TestReadEOF(t *testing.T) {
	t.Parallel()
	full := maskedFrame(opBinary, "hello")
	for _, cut := range []int{0, 1, 3, len(full) - 1} {
		transport := &fuzzTransport{Reader: bytes.NewReader(full[:cut])}
		conn := newConn(transport, bufio.NewReader(transport), false)
		var dst bytes.Buffer
		_, err := conn.ReadMessage(&dst, -1)
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF, assert.Sprintf("cut at %d", cut))
		assert.True(t, transport.closed)
		_, again := conn.ReadMessage(&dst, -1)
		assert.ErrorIs(t, again, io.ErrUnexpectedEOF)
	}
}

// TestInvalidCloseFrame closes the transport at once for a Close frame whose
// header is invalid. The peer has closed and sends nothing more to drain.
func TestInvalidCloseFrame(t *testing.T) {
	t.Parallel()
	transport := &fuzzTransport{Reader: bytes.NewReader(maskedFrame(opClose, string(make([]byte, 126))))}
	conn := newConn(transport, bufio.NewReader(transport), false)
	_, err := conn.ReadMessage(&bytes.Buffer{}, -1)
	assert.ErrorIs(t, err, ErrProtocol)
	assert.True(t, transport.closed)
	_, again := conn.ReadMessage(&bytes.Buffer{}, -1)
	assert.ErrorIs(t, again, ErrProtocol)
}

// TestDiscardLongFrame discards a frame whose length doesn't fit a 32-bit
// int. A wrapped length would skip 3 bytes and read the next frame.
func TestDiscardLongFrame(t *testing.T) {
	t.Parallel()
	input := []byte{0x80 | byte(opBinary), 0x80 | 127}
	input = binary.BigEndian.AppendUint64(input, 1<<32+3)
	input = append(input, 0, 0, 0, 0)
	input = append(input, "abc"...)
	input = append(input, maskedFrame(opText, "hi")...)
	transport := &fuzzTransport{Reader: bytes.NewReader(input)}
	conn := newConn(transport, bufio.NewReader(transport), false)
	_, err := conn.ReadMessage(nil, -1)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.True(t, transport.closed)
}

func TestCloseUnblocks(t *testing.T) {
	t.Parallel()
	t.Run("read", func(t *testing.T) {
		t.Parallel()
		_, server, _ := newPipePair(t)
		readErr := make(chan error, 1)
		go func() {
			_, err := server.ReadMessage(&bytes.Buffer{}, -1)
			readErr <- err
		}()
		assert.Nil(t, server.Close())
		assert.ErrorIs(t, <-readErr, net.ErrClosed)
		assert.Nil(t, server.Close())
	})
	t.Run("write", func(t *testing.T) {
		t.Parallel()
		client, _, writing := newPipePair(t)
		writeErr := make(chan error, 1)
		go func() { writeErr <- client.WriteMessage(MessageBinary, frameOf(make([]byte, 1<<16))) }()
		<-writing
		assert.Nil(t, client.Close())
		assert.ErrorIs(t, <-writeErr, net.ErrClosed)
		assert.ErrorIs(t, client.Ping(), net.ErrClosed)
	})
}

func TestDrainBudget(t *testing.T) {
	t.Parallel()
	closeFrame := maskedFrame(opClose, "\x03\xe8")
	t.Run("after_failure", func(t *testing.T) {
		t.Parallel()
		for _, size := range []int{1 << 10, 80 << 10} {
			input := maskedFrame(opBinary, string(make([]byte, 17)))
			input = append(input, maskedFrame(opBinary, string(make([]byte, size)))...)
			input = append(input, closeFrame...)
			reader := bytes.NewReader(input)
			transport := &fuzzTransport{Reader: reader}
			conn := newConn(transport, bufio.NewReader(transport), false)
			_, err := conn.ReadMessage(&bytes.Buffer{}, 16)
			assert.ErrorIs(t, err, ErrMessageTooBig)
			_, again := conn.ReadMessage(nil, -1)
			assert.ErrorIs(t, again, ErrMessageTooBig)
			assert.True(t, transport.closed)
			// Only a message within the budget lets the drain reach the Close.
			assert.Equal(t, reader.Len() == 0, size < minDrainBudget, assert.Sprintf("size %d", size))
		}
	})
	t.Run("after_close", func(t *testing.T) {
		t.Parallel()
		for _, limit := range []int64{-1, 100 << 10} {
			input := maskedFrame(opBinary, "ok")
			input = append(input, maskedFrame(opBinary, string(make([]byte, 80<<10)))...)
			input = append(input, closeFrame...)
			transport := &fuzzTransport{Reader: bytes.NewReader(input)}
			conn := newConn(transport, bufio.NewReader(transport), false)
			_, err := conn.ReadMessage(&bytes.Buffer{}, limit)
			assert.Nil(t, err)
			assert.Nil(t, conn.WriteClose(StatusNormalClosure, ""))
			_, err = conn.ReadMessage(&bytes.Buffer{}, -1)
			// The largest limit seen raises the budget past the message.
			if limit > 0 {
				assert.Equal(t, err, error(&CloseError{Code: StatusNormalClosure}))
			} else {
				assert.ErrorIs(t, err, net.ErrClosed)
			}
			assert.True(t, transport.closed)
		}
	})
}

func BenchmarkReadWrite(b *testing.B) {
	for _, writer := range []string{"client", "server"} {
		for _, size := range []int{16, 1 << 10, 64 << 10} {
			b.Run(fmt.Sprintf("%s_writes/%d", writer, size), func(b *testing.B) {
				client, server, _ := newPipePair(b)
				from, to := client, server
				if writer == "server" {
					from, to = server, client
				}
				payload := bytes.Repeat([]byte{'x'}, size)
				var headerSpace [MaxHeaderSize]byte
				writeErr := make(chan error, 1)
				b.ReportAllocs()
				b.SetBytes(int64(size))
				b.ResetTimer()
				go func() {
					var frame bytes.Buffer
					for range b.N {
						frame.Reset()
						frame.Write(headerSpace[:])
						frame.Write(payload)
						if err := from.WriteMessage(MessageBinary, frame.Bytes()); err != nil {
							writeErr <- err
							return
						}
					}
					writeErr <- nil
				}()
				var dst bytes.Buffer
				for range b.N {
					dst.Reset()
					if _, err := to.ReadMessage(&dst, -1); err != nil {
						b.Fatal(err)
					}
				}
				if err := <-writeErr; err != nil {
					b.Fatal(err)
				}
			})
		}
	}
}

// BenchmarkPing is one server PING and a 16 B message. The client answers
// the PING inside ReadMessage and the server's reader drops the PONG.
func BenchmarkPing(b *testing.B) {
	client, server, _ := newPipePair(b)
	go func() {
		for {
			if _, err := server.ReadMessage(nil, -1); err != nil {
				return
			}
		}
	}()
	frame := frameOf(make([]byte, 16))
	writeErr := make(chan error, 1)
	b.ReportAllocs()
	b.ResetTimer()
	go func() {
		for range b.N {
			if err := server.Ping(); err != nil {
				writeErr <- err
				return
			}
			if err := server.WriteMessage(MessageBinary, frame); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()
	var dst bytes.Buffer
	for range b.N {
		dst.Reset()
		if _, err := client.ReadMessage(&dst, -1); err != nil {
			b.Fatal(err)
		}
	}
	if err := <-writeErr; err != nil {
		b.Fatal(err)
	}
}

// BenchmarkCompression sends JSON-like messages with and without
// permessage-deflate.
func BenchmarkCompression(b *testing.B) {
	for _, mode := range []string{"plain", "deflate"} {
		for _, writer := range []string{"client", "server"} {
			for _, size := range []int{1 << 10, 64 << 10} {
				b.Run(fmt.Sprintf("%s/%s_writes/%d", mode, writer, size), func(b *testing.B) {
					client, server, _ := newPipePair(b)
					for _, conn := range []*Conn{client, server} {
						conn.deflate = mode == "deflate"
						conn.threshold = defaultCompressionThreshold
					}
					from, to := client, server
					if writer == "server" {
						from, to = server, client
					}
					payload := jsonPayload(size)
					var headerSpace [MaxHeaderSize]byte
					writeErr := make(chan error, 1)
					b.ReportAllocs()
					b.SetBytes(int64(size))
					b.ResetTimer()
					go func() {
						var frame bytes.Buffer
						for range b.N {
							frame.Reset()
							frame.Write(headerSpace[:])
							frame.Write(payload)
							if err := from.WriteMessage(MessageText, frame.Bytes()); err != nil {
								writeErr <- err
								return
							}
						}
						writeErr <- nil
					}()
					var dst bytes.Buffer
					for range b.N {
						dst.Reset()
						if _, err := to.ReadMessage(&dst, -1); err != nil {
							b.Fatal(err)
						}
					}
					if err := <-writeErr; err != nil {
						b.Fatal(err)
					}
				})
			}
		}
	}
}

// newPipePair connects a client and a server Conn over net.Pipe. The channel
// closes when the client starts a Write of at least 64 KiB.
func newPipePair(tb testing.TB) (*Conn, *Conn, <-chan struct{}) {
	tb.Helper()
	clientPipe, serverPipe := net.Pipe()
	writing := make(chan struct{})
	transport := &notifyConn{Conn: clientPipe, writing: writing}
	client := newConn(transport, bufio.NewReader(transport), true)
	server := newConn(serverPipe, bufio.NewReader(serverPipe), false)
	tb.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server, writing
}

func frameOf(payload []byte) []byte {
	return append(make([]byte, MaxHeaderSize, MaxHeaderSize+len(payload)), payload...)
}

// notifyConn closes writing when a large Write starts.
type notifyConn struct {
	net.Conn

	writing chan struct{}
	once    sync.Once
}

func (c *notifyConn) Write(data []byte) (int, error) {
	if len(data) >= 1<<16 {
		c.once.Do(func() { close(c.writing) })
	}
	return c.Conn.Write(data)
}

// loopTransport reads data over and over and discards writes.
type loopTransport struct {
	data []byte
	pos  int
}

func (l *loopTransport) Read(data []byte) (int, error) {
	if len(l.data) == 0 {
		return 0, io.EOF
	}
	size := copy(data, l.data[l.pos:])
	l.pos = (l.pos + size) % len(l.data)
	return size, nil
}

func (*loopTransport) Write(data []byte) (int, error) {
	return len(data), nil
}

func (*loopTransport) Close() error {
	return nil
}

// bufferTransport records writes.
type bufferTransport struct {
	buffer *bytes.Buffer
}

func (*bufferTransport) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (b *bufferTransport) Write(data []byte) (int, error) {
	return b.buffer.Write(data)
}

func (*bufferTransport) Close() error {
	return nil
}
