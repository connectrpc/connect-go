// Copyright 2021-2025 The Connect Authors
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

package memhttp

// This file implements an in-memory net.Conn pair inspired by the proposed
// testing/nettest.Conn in https://go.dev/issue/77362. When that proposal is
// accepted and available in the minimum Go version required by this module,
// this implementation can be replaced with the upstream one.

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// defaultConnBufferSize is the default maximum buffer size per direction.
// High enough to avoid blocking in normal tests, low enough to prevent
// OOM if a test has a runaway writer.
const defaultConnBufferSize = 1 << 22 // 4MB

var errClosedByPeer = errors.New("connection closed by peer")

// memoryConn is an in-memory implementation of net.Conn.
// Connections come in pairs: writes to one side are read by the other.
type memoryConn struct {
	// A memoryConn reads from r and writes to w.
	// Its peer has the same halves, swapped.
	r, w *connHalf
}

// newMemoryConnPair returns a connected pair of in-memory connections.
// Writes to one are readable from the other, and vice versa. The bufSize
// parameter controls the maximum bytes buffered per direction. If a writer
// exceeds this limit it blocks until the reader drains enough data.
func newMemoryConnPair(addr memoryAddr, bufSize int) (*memoryConn, *memoryConn) {
	half1 := newConnHalf(addr, bufSize)
	half2 := newConnHalf(addr, bufSize)
	conn1 := &memoryConn{r: half1, w: half2}
	conn2 := &memoryConn{r: half2, w: half1}
	return conn1, conn2
}

// Read reads data from the connection.
func (c *memoryConn) Read(data []byte) (int, error) {
	bytesRead, err := c.r.read(data)
	if err != nil && !errors.Is(err, io.EOF) {
		err = &net.OpError{
			Op:     "read",
			Net:    c.LocalAddr().Network(),
			Source: c.RemoteAddr(),
			Addr:   c.LocalAddr(),
			Err:    err,
		}
	}
	return bytesRead, err
}

// Write writes data to the connection.
func (c *memoryConn) Write(data []byte) (int, error) {
	bytesWritten, err := c.w.write(data)
	if err != nil {
		err = &net.OpError{
			Op:     "write",
			Net:    c.LocalAddr().Network(),
			Source: c.LocalAddr(),
			Addr:   c.RemoteAddr(),
			Err:    err,
		}
	}
	return bytesWritten, err
}

// CloseRead shuts down the reading side of the connection. Any blocked
// reads are unblocked and return errors. Buffered data remains
// available for reading; the error is returned once the buffer is
// drained. This matches TCP behavior where a close does not discard
// data already in the receive buffer.
func (c *memoryConn) CloseRead() error {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	c.r.readErr = net.ErrClosed
	c.r.writeErr = errClosedByPeer
	c.r.readCond.Broadcast()
	c.r.writeCond.Broadcast()
	return nil
}

// CloseWrite shuts down the writing side of the connection.
func (c *memoryConn) CloseWrite() error {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	if c.w.readErr == nil {
		c.w.readErr = io.EOF
	}
	c.w.writeErr = net.ErrClosed
	c.w.readCond.Broadcast()
	c.w.writeCond.Broadcast()
	return nil
}

// Close closes the connection. Any blocked Read or Write operations
// will be unblocked and return errors.
func (c *memoryConn) Close() error {
	_ = c.CloseWrite()
	_ = c.CloseRead()
	return nil
}

// LocalAddr returns the local network address.
func (c *memoryConn) LocalAddr() net.Addr {
	return c.r.addr
}

// RemoteAddr returns the remote network address.
func (c *memoryConn) RemoteAddr() net.Addr {
	return c.w.addr
}

// SetDeadline sets both read and write deadlines.
func (c *memoryConn) SetDeadline(deadline time.Time) error {
	_ = c.SetReadDeadline(deadline)
	_ = c.SetWriteDeadline(deadline)
	return nil
}

// SetReadDeadline sets the read deadline.
func (c *memoryConn) SetReadDeadline(deadline time.Time) error {
	c.r.setReadDeadline(deadline)
	return nil
}

// SetWriteDeadline sets the write deadline.
func (c *memoryConn) SetWriteDeadline(deadline time.Time) error {
	c.w.setWriteDeadline(deadline)
	return nil
}

// connHalf is one direction of data flow in a memoryConn.
// Writes push data into the buffer; reads pull from it.
//
// A single sync.Mutex protects all state. Two sync.Cond variables allow
// readers and writers to block independently:
//   - readCond is broadcast when data becomes available, readErr is set,
//     or the read deadline expires.
//   - writeCond is broadcast when buffer space is freed, writeErr is set,
//     or the write deadline expires.
type connHalf struct {
	addr memoryAddr

	mu        sync.Mutex
	readCond  sync.Cond
	writeCond sync.Cond

	readDeadline  connDeadline
	writeDeadline connDeadline

	bufMax int
	buf    bytes.Buffer

	readErr  error
	writeErr error
}

func newConnHalf(addr memoryAddr, bufMax int) *connHalf {
	half := &connHalf{
		addr:   addr,
		bufMax: bufMax,
	}
	half.readCond.L = &half.mu
	half.writeCond.L = &half.mu
	return half
}

func (half *connHalf) read(data []byte) (int, error) {
	half.mu.Lock()
	defer half.mu.Unlock()
	for half.buf.Len() == 0 && half.readErr == nil && !half.readDeadline.expired {
		half.readCond.Wait()
	}
	if half.readDeadline.expired {
		return 0, os.ErrDeadlineExceeded
	}
	if half.buf.Len() == 0 && half.readErr != nil {
		return 0, half.readErr
	}
	bytesRead, err := half.buf.Read(data)
	half.writeCond.Broadcast() // buffer space freed, wake blocked writers
	return bytesRead, err
}

func (half *connHalf) write(data []byte) (int, error) {
	var written int
	for written < len(data) {
		bytesWritten, err := half.writePartial(data[written:])
		written += bytesWritten
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (half *connHalf) writePartial(data []byte) (int, error) {
	half.mu.Lock()
	defer half.mu.Unlock()
	for half.buf.Len() >= half.bufMax && half.writeErr == nil && !half.writeDeadline.expired {
		half.writeCond.Wait()
	}
	if half.writeDeadline.expired {
		return 0, os.ErrDeadlineExceeded
	}
	if half.writeErr != nil {
		return 0, half.writeErr
	}
	writeMax := half.bufMax - half.buf.Len()
	if writeMax < len(data) {
		data = data[:writeMax]
	}
	bytesWritten, err := half.buf.Write(data)
	half.readCond.Broadcast() // data available, wake blocked readers
	return bytesWritten, err
}

func (half *connHalf) setReadDeadline(deadline time.Time) {
	half.readDeadline.setDeadline(&half.mu, &half.readCond, deadline)
}

func (half *connHalf) setWriteDeadline(deadline time.Time) {
	half.writeDeadline.setDeadline(&half.mu, &half.writeCond, deadline)
}

// connDeadline tracks a deadline timer and its expired state.
type connDeadline struct {
	timer   *time.Timer
	expired bool
}

// setDeadline updates the deadline.
func (d *connDeadline) setDeadline(mutex *sync.Mutex, cond *sync.Cond, deadline time.Time) {
	mutex.Lock()
	defer mutex.Unlock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	if deadline.IsZero() {
		d.expired = false
		return
	}
	expiry := time.Until(deadline)
	if expiry <= 0 {
		d.expired = true
		cond.Broadcast()
		return
	}
	d.expired = false
	var timer *time.Timer
	timer = time.AfterFunc(expiry, func() {
		mutex.Lock()
		defer mutex.Unlock()
		if d.timer == timer {
			d.timer = nil
			d.expired = true
			cond.Broadcast()
		}
	})
	d.timer = timer
}
