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
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
)

const (
	// minDrainBudget is the least data a Conn discards after sending a Close
	// frame. The largest read limit seen raises it.
	minDrainBudget = 64 << 10
	// maxGrow caps how far ReadMessage grows dst ahead of a frame's payload,
	// leaving a hostile length unable to force a large allocation.
	maxGrow = 1 << 20
)

var errShortFrame = errors.New("websocket: frame is shorter than MaxHeaderSize")

// Conn is a WebSocket connection. Every method is safe for concurrent use.
// ReadMessage calls run one at a time.
type Conn struct {
	transport io.ReadWriteCloser
	reader    *bufio.Reader
	client    bool

	readMu sync.Mutex
	// Guarded by readMu.
	frame       frameReader
	readErr     error
	drainNext   bool  // readErr sent a Close frame and the next call drains
	maxLimit    int64 // the largest limit passed to ReadMessage
	discarded   int64 // data bytes discarded after this side's Close frame
	controlRead [maxControlPayload]byte

	writeMu sync.Mutex
	// Guarded by writeMu.
	controlWrite [MaxHeaderSize + maxControlPayload]byte

	pongMu sync.Mutex
	// Guarded by pongMu.
	pongPayload [maxControlPayload]byte
	pongLength  int
	pongPending atomic.Bool

	closeSent atomic.Bool
	closed    atomic.Bool
	closeOnce sync.Once
}

// ReadMessage appends the next data message to dst and returns its type. A nil
// dst discards the message without validating its UTF-8. limit bounds the
// message's size, not counting bytes already in dst. A negative limit accepts
// any size. On error dst may hold part of a message.
//
// ReadMessage answers PINGs, ignores PONGs and runs the close handshake on the
// way. With no write in progress it writes the PONG itself, which blocks until
// the transport takes it or the Conn is closed. Once this side has sent a Close frame it discards data messages and
// returns only when the connection ends.
//
// Every error leaves the Conn either closed or with a Close frame sent. When
// the error sent a Close frame (ErrProtocol, ErrInvalidUTF8,
// ErrMessageTooBig), the next call drains. It reads until the peer's Close
// frame, the transport ends or a byte budget runs out, closes the transport
// and returns the same error. After any other error, and on every later call,
// it returns the error at once.
func (c *Conn) ReadMessage(dst *bytes.Buffer, limit int64) (MessageType, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.readErr != nil {
		if c.drainNext {
			c.drainNext = false
			c.drain()
		}
		return 0, c.readErr
	}
	c.maxLimit = max(c.maxLimit, limit)
	var messageType MessageType
	var total int64
	dropped := false
	for {
		hdr, err := readHeader(c.reader)
		if err != nil {
			return 0, c.readFailed(err)
		}
		c.frame.remaining = hdr.length
		if err := hdr.check(c.client); err != nil {
			err = fmt.Errorf("%w: %w", ErrProtocol, err)
			if hdr.opcode == opClose {
				// The peer has closed, which leaves nothing to drain.
				_, _ = c.writeClose(StatusProtocolError, "")
				_ = c.Close()
				c.readErr = err
				return 0, err
			}
			return 0, c.fail(StatusProtocolError, err)
		}
		if hdr.opcode.isControl() {
			if err := c.readControl(hdr); err != nil {
				return 0, err
			}
			continue
		}
		switch {
		case hdr.opcode == opContinuation && messageType == 0:
			return 0, c.fail(StatusProtocolError, fmt.Errorf("%w: continuation frame without a message", ErrProtocol))
		case hdr.opcode != opContinuation && messageType != 0:
			return 0, c.fail(StatusProtocolError, fmt.Errorf("%w: data frame inside a fragmented message", ErrProtocol))
		case hdr.opcode != opContinuation:
			messageType = MessageType(hdr.opcode)
			c.frame.text = messageType == MessageText
			c.frame.utf8 = utf8Validator{}
		}
		if c.closeSent.Load() {
			dropped = true
			if c.discarded += hdr.length; c.discarded > c.drainBudget() {
				_ = c.Close()
				c.readErr = fmt.Errorf("websocket: peer sent over %d bytes after Close: %w", c.drainBudget(), net.ErrClosed)
				return 0, c.readErr
			}
		} else if limit >= 0 && hdr.length > limit-total {
			return 0, c.fail(StatusMessageTooBig, fmt.Errorf("%w: limit is %d bytes", ErrMessageTooBig, limit))
		}
		total += hdr.length
		if err := c.readPayload(dst, hdr, dst == nil || dropped); err != nil {
			return 0, err
		}
		if !hdr.fin {
			continue
		}
		if dropped {
			messageType, total, dropped = 0, 0, false
			continue
		}
		if dst != nil && c.frame.text && !c.frame.utf8.complete() {
			return 0, c.fail(StatusInvalidPayloadData, ErrInvalidUTF8)
		}
		return messageType, nil
	}
}

// WriteMessage sends frame[MaxHeaderSize:] as one final frame with a single
// Write to the transport. It writes the header into frame[:MaxHeaderSize].
// A client masks the payload in place. The frame's bytes are unusable after
// the call. A frame shorter than MaxHeaderSize is an error. A failed write
// closes the transport. Writes after a Close frame return an error wrapping
// [net.ErrClosed].
func (c *Conn) WriteMessage(messageType MessageType, frame []byte) error {
	if len(frame) < MaxHeaderSize {
		return errShortFrame
	}
	if messageType != MessageText && messageType != MessageBinary {
		return fmt.Errorf("websocket: invalid message type %d", messageType)
	}
	c.writeMu.Lock()
	err := c.writeFrameLocked(opcode(messageType), frame)
	c.unlockWrite()
	return err
}

// Ping sends an empty PING. It doesn't wait for the PONG.
func (c *Conn) Ping() error {
	c.writeMu.Lock()
	err := c.writeFrameLocked(opPing, c.controlWrite[:MaxHeaderSize])
	c.unlockWrite()
	return err
}

// WriteClose sends a Close frame with code and reason, truncating reason to
// 123 bytes on a UTF-8 boundary. StatusNoStatusReceived sends a Close frame
// without a code. If a Close frame was already sent it does nothing and
// returns nil. It doesn't wait for the peer. ReadMessage completes the
// handshake.
func (c *Conn) WriteClose(code StatusCode, reason string) error {
	_, err := c.writeClose(code, reason)
	return err
}

// Close closes the transport at once, unblocking any read or write. It is
// idempotent.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		err = c.transport.Close()
	})
	if err != nil {
		return fmt.Errorf("websocket: close: %w", err)
	}
	return nil
}

// discard skips size bytes of the transport. It steps in chunks that fit an
// int on 32-bit platforms, where converting a 64-bit frame length would wrap.
func (c *Conn) discard(size int64) error {
	for size > 0 {
		step := int(min(size, math.MaxInt32))
		if _, err := c.reader.Discard(step); err != nil {
			return err
		}
		size -= int64(step)
	}
	return nil
}

// drain discards frames until the peer's Close, the transport ends or the
// budget runs out, then closes the transport. The caller holds readMu.
func (c *Conn) drain() {
	defer func() { _ = c.Close() }()
	budget := c.drainBudget() - c.frame.remaining
	if c.frame.remaining < 0 || budget < 0 {
		return
	}
	if c.discard(c.frame.remaining) != nil {
		return
	}
	for {
		hdr, err := readHeader(c.reader)
		if err != nil || hdr.check(c.client) != nil || hdr.opcode == opClose {
			return
		}
		if budget -= hdr.length; budget < 0 {
			return
		}
		if c.discard(hdr.length) != nil {
			return
		}
	}
}

// drainBudget is how much data the Conn discards after its Close frame.
func (c *Conn) drainBudget() int64 {
	return max(minDrainBudget, c.maxLimit)
}

// fail sends a Close frame with code and makes the next ReadMessage drain.
// If this side already sent one it closes the transport instead.
func (c *Conn) fail(code StatusCode, cause error) error {
	sent, _ := c.writeClose(code, "")
	c.readErr = cause
	if sent {
		c.drainNext = true
	} else {
		_ = c.Close()
	}
	return cause
}

// flushPongLocked writes the pending PONG. The caller holds writeMu.
func (c *Conn) flushPongLocked() {
	// Clearing the flag before the copy lets a PING that lands mid-copy set
	// it again for the next turn of unlockWrite.
	if !c.pongPending.Swap(false) || c.closeSent.Load() {
		return
	}
	c.pongMu.Lock()
	frame := append(c.controlWrite[:MaxHeaderSize], c.pongPayload[:c.pongLength]...)
	c.pongMu.Unlock()
	_ = c.writeFrameLocked(opPong, frame)
}

// queuePong records a PONG for payload and sends it unless a writer holds
// writeMu, in which case that writer sends it before it unlocks. A newer PING
// replaces an unsent PONG, as RFC 6455 Section 5.5.3 allows.
func (c *Conn) queuePong(payload []byte) {
	if c.closeSent.Load() {
		return
	}
	c.pongMu.Lock()
	c.pongLength = copy(c.pongPayload[:], payload)
	c.pongMu.Unlock()
	c.pongPending.Store(true)
	if c.writeMu.TryLock() {
		c.unlockWrite()
	}
}

// readControl reads a control frame's payload and acts on it. The caller
// holds readMu.
func (c *Conn) readControl(hdr header) error {
	payload := c.controlRead[:hdr.length]
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return c.readFailed(err)
	}
	c.frame.remaining = 0
	if hdr.masked {
		mask(payload, hdr.maskKey, 0)
	}
	if hdr.opcode == opPing {
		c.queuePong(payload)
		return nil
	}
	if hdr.opcode == opPong {
		return nil
	}
	closeErr, failCode, err := parseClose(payload)
	if err != nil {
		// The peer has closed, which leaves nothing to drain.
		_, _ = c.writeClose(failCode, "")
		_ = c.Close()
		c.readErr = err
		return err
	}
	_, _ = c.writeClose(closeErr.Code, "")
	_ = c.Close()
	c.readErr = closeErr
	return closeErr
}

// readFailed records a transport error and closes the transport.
func (c *Conn) readFailed(err error) error {
	switch {
	case c.closed.Load():
		err = net.ErrClosed
	case errors.Is(err, io.EOF):
		err = io.ErrUnexpectedEOF
	}
	c.readErr = fmt.Errorf("websocket: read: %w", err)
	_ = c.Close()
	return c.readErr
}

// readPayload appends a data frame's payload to dst, or discards it.
func (c *Conn) readPayload(dst *bytes.Buffer, hdr header, discard bool) error {
	if discard {
		if err := c.discard(hdr.length); err != nil {
			return c.readFailed(err)
		}
		c.frame.remaining = 0
		return nil
	}
	if hdr.length == 0 {
		return nil
	}
	c.frame.masked = hdr.masked
	c.frame.maskKey = hdr.maskKey
	c.frame.maskPos = 0
	// The extra MinRead covers ReadFrom's first grow, which would otherwise
	// reallocate a buffer grown to exactly the frame's length.
	dst.Grow(int(min(hdr.length, maxGrow)) + bytes.MinRead)
	if _, err := dst.ReadFrom(&c.frame); err != nil {
		if errors.Is(err, ErrInvalidUTF8) {
			return c.fail(StatusInvalidPayloadData, ErrInvalidUTF8)
		}
		return c.readFailed(err)
	}
	return nil
}

// unlockWrite sends a pending PONG before it lets go of writeMu, and takes it
// back if a PING arrived after the check. Every path that takes writeMu
// releases it here, which never loses a PONG.
func (c *Conn) unlockWrite() {
	for {
		if c.pongPending.Load() {
			c.flushPongLocked()
		}
		c.writeMu.Unlock()
		if !c.pongPending.Load() || !c.writeMu.TryLock() {
			return
		}
	}
}

// writeClose sends a Close frame unless one was sent, and reports whether it
// sent one.
func (c *Conn) writeClose(code StatusCode, reason string) (bool, error) {
	if code != StatusNoStatusReceived && !validCloseCode(code) {
		return false, fmt.Errorf("websocket: invalid close code %d", code)
	}
	c.writeMu.Lock()
	defer c.unlockWrite()
	if c.closeSent.Load() {
		return false, nil
	}
	frame := c.controlWrite[:MaxHeaderSize]
	if code != StatusNoStatusReceived {
		frame = binary.BigEndian.AppendUint16(frame, uint16(code))
		frame = append(frame, closeReason(reason)...)
	}
	err := c.writeFrameLocked(opClose, frame)
	c.closeSent.Store(true)
	return err == nil, err
}

// writeFrameLocked sends a final frame with one Write. The caller holds
// writeMu.
func (c *Conn) writeFrameLocked(frameOpcode opcode, frame []byte) error {
	if c.closeSent.Load() || c.closed.Load() {
		return fmt.Errorf("websocket: write: %w", net.ErrClosed)
	}
	payload := frame[MaxHeaderSize:]
	start := putHeader(frame[:MaxHeaderSize], frameOpcode, len(payload), c.client)
	if c.client {
		mask(payload, [4]byte(frame[MaxHeaderSize-4:MaxHeaderSize]), 0)
	}
	if _, err := c.transport.Write(frame[start:]); err != nil {
		// A partial frame poisons the stream.
		if c.closed.Load() {
			err = net.ErrClosed
		}
		_ = c.Close()
		return fmt.Errorf("websocket: write: %w", err)
	}
	return nil
}

// frameReader reads the payload of the current data frame, unmasking it and
// validating text as it goes.
type frameReader struct {
	reader    *bufio.Reader
	remaining int64
	masked    bool
	maskKey   [4]byte
	maskPos   int
	text      bool
	utf8      utf8Validator
}

// Read returns io.EOF with the frame's last bytes, which ends
// bytes.Buffer.ReadFrom without another grow. A transport that ends mid-frame
// returns io.ErrUnexpectedEOF, which ReadFrom doesn't take for success.
func (f *frameReader) Read(data []byte) (int, error) {
	if f.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(data)) > f.remaining {
		data = data[:f.remaining]
	}
	size, err := f.reader.Read(data)
	data = data[:size]
	f.remaining -= int64(size)
	if f.masked {
		f.maskPos = mask(data, f.maskKey, f.maskPos)
	}
	if f.text && !f.utf8.valid(data) {
		return size, ErrInvalidUTF8
	}
	switch {
	case errors.Is(err, io.EOF) && f.remaining > 0:
		return size, io.ErrUnexpectedEOF
	case err != nil:
		return size, err
	case f.remaining == 0:
		return size, io.EOF
	}
	return size, nil
}

func newConn(transport io.ReadWriteCloser, reader *bufio.Reader, client bool) *Conn {
	conn := &Conn{transport: transport, reader: reader, client: client}
	conn.frame.reader = reader
	return conn
}
