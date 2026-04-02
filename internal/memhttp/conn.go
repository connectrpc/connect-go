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
	h1 := newConnHalf(addr, bufSize)
	h2 := newConnHalf(addr, bufSize)
	c1 := &memoryConn{r: h1, w: h2}
	c2 := &memoryConn{r: h2, w: h1}
	return c1, c2
}

// Read reads data from the connection.
func (c *memoryConn) Read(b []byte) (int, error) {
	n, err := c.r.read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		err = &net.OpError{
			Op:     "read",
			Net:    c.LocalAddr().Network(),
			Source: c.RemoteAddr(),
			Addr:   c.LocalAddr(),
			Err:    err,
		}
	}
	return n, err
}

// Write writes data to the connection.
func (c *memoryConn) Write(b []byte) (int, error) {
	n, err := c.w.write(b)
	if err != nil {
		err = &net.OpError{
			Op:     "write",
			Net:    c.LocalAddr().Network(),
			Source: c.LocalAddr(),
			Addr:   c.RemoteAddr(),
			Err:    err,
		}
	}
	return n, err
}

// CloseRead shuts down the reading side of the connection.
func (c *memoryConn) CloseRead() error {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	c.r.buf.Reset()
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

// Close closes both sides of the connection.
func (c *memoryConn) Close() error {
	c.CloseRead()
	c.CloseWrite()
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
func (c *memoryConn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	c.SetWriteDeadline(t)
	return nil
}

// SetReadDeadline sets the read deadline.
func (c *memoryConn) SetReadDeadline(t time.Time) error {
	c.r.setReadDeadline(t)
	return nil
}

// SetWriteDeadline sets the write deadline.
func (c *memoryConn) SetWriteDeadline(t time.Time) error {
	c.w.setWriteDeadline(t)
	return nil
}

var errClosedByPeer = errors.New("connection closed by peer")

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
	h := &connHalf{
		addr:   addr,
		bufMax: bufMax,
	}
	h.readCond.L = &h.mu
	h.writeCond.L = &h.mu
	return h
}

func (h *connHalf) read(b []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for h.buf.Len() == 0 && h.readErr == nil && !h.readDeadline.expired {
		h.readCond.Wait()
	}
	if h.readDeadline.expired {
		return 0, os.ErrDeadlineExceeded
	}
	if h.buf.Len() == 0 && h.readErr != nil {
		return 0, h.readErr
	}
	n, err := h.buf.Read(b)
	h.writeCond.Broadcast() // buffer space freed, wake blocked writers
	return n, err
}

func (h *connHalf) write(b []byte) (int, error) {
	var n int
	for n < len(b) {
		nn, err := h.writePartial(b[n:])
		n += nn
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (h *connHalf) writePartial(b []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for h.buf.Len() >= h.bufMax && h.writeErr == nil && !h.writeDeadline.expired {
		h.writeCond.Wait()
	}
	if h.writeDeadline.expired {
		return 0, os.ErrDeadlineExceeded
	}
	if h.writeErr != nil {
		return 0, h.writeErr
	}
	writeMax := h.bufMax - h.buf.Len()
	if writeMax < len(b) {
		b = b[:writeMax]
	}
	n, err := h.buf.Write(b)
	h.readCond.Broadcast() // data available, wake blocked readers
	return n, err
}

func (h *connHalf) setReadDeadline(t time.Time) {
	h.readDeadline.setDeadline(&h.mu, &h.readCond, t)
}

func (h *connHalf) setWriteDeadline(t time.Time) {
	h.writeDeadline.setDeadline(&h.mu, &h.writeCond, t)
}

// connDeadline tracks a deadline timer and its expired state.
type connDeadline struct {
	timer   *time.Timer
	expired bool
}

// setDeadline updates the deadline.
func (d *connDeadline) setDeadline(mu *sync.Mutex, cond *sync.Cond, t time.Time) {
	mu.Lock()
	defer mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	if t.IsZero() {
		d.expired = false
		return
	}
	expiry := time.Until(t)
	if expiry <= 0 {
		d.expired = true
		cond.Broadcast()
		return
	}
	d.expired = false
	var timer *time.Timer
	timer = time.AfterFunc(expiry, func() {
		mu.Lock()
		defer mu.Unlock()
		if d.timer == timer {
			d.timer = nil
			d.expired = true
			cond.Broadcast()
		}
	})
	d.timer = timer
}
