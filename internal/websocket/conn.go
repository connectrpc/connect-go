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

var (
	errShortFrame = errors.New("websocket: frame is shorter than MaxHeaderSize")
	// errDropped stops reading a message that arrived after this side's Close
	// frame.
	errDropped = errors.New("websocket: message arrived after close")
)

// Conn is a WebSocket connection. Every method is safe for concurrent use.
// ReadMessage calls run one at a time.
type Conn struct {
	transport io.ReadWriteCloser
	reader    *bufio.Reader
	client    bool
	deflate   bool // permessage-deflate was negotiated
	threshold int  // the smallest payload WriteMessage compresses

	readMu sync.Mutex
	// Guarded by readMu.
	message     messageReader
	inflate     inflater
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
// message's size, not counting bytes already in dst. For a compressed message
// it bounds the inflated size, and the compressed payload may be at most
// twice limit plus 1 KiB. A negative limit accepts any size. On error dst may
// hold part of a message.
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
	for {
		hdr, err := c.readDataHeader(false)
		if err != nil {
			return 0, err
		}
		messageType := MessageType(hdr.opcode)
		compressed := hdr.rsv&rsv1 != 0
		c.message.begin(messageType == MessageText && !compressed, compressed, limit)
		if err := c.startFrame(hdr); err != nil {
			return 0, err
		}
		switch {
		case dst == nil || c.message.dropped:
			err = c.skipMessage()
		case compressed:
			err = c.inflateMessage(dst, messageType == MessageText, limit)
		default:
			err = c.readPlain(dst)
		}
		switch {
		case err == nil && c.message.dropped:
			continue
		case err == nil:
			return messageType, nil
		case c.readErr != nil:
			return 0, c.readErr
		case c.message.dropped:
			if err := c.skipMessage(); err != nil {
				return 0, err
			}
			continue
		}
		return 0, c.failMessage(err)
	}
}

// WriteMessage sends frame[MaxHeaderSize:] as one final frame with a single
// Write to the transport. It writes the header into frame[:MaxHeaderSize].
// A client masks the payload in place. The frame's bytes are unusable after
// the call. A frame shorter than MaxHeaderSize is an error. A failed write
// closes the transport. Writes after a Close frame return an error wrapping
// [net.ErrClosed].
//
// With compression negotiated, a payload of at least the threshold is
// deflated into a pooled buffer before the write starts, and sent from there
// if that made it smaller.
func (c *Conn) WriteMessage(messageType MessageType, frame []byte) error {
	if len(frame) < MaxHeaderSize {
		return errShortFrame
	}
	if messageType != MessageText && messageType != MessageBinary {
		return fmt.Errorf("websocket: invalid message type %d", messageType)
	}
	if c.deflate && len(frame)-MaxHeaderSize >= c.threshold {
		compressor := getCompressor()
		defer putCompressor(compressor)
		if compressed, ok := compressor.compress(frame[MaxHeaderSize:]); ok {
			return c.writeMessage(opcode(messageType), rsv1, compressed)
		}
	}
	return c.writeMessage(opcode(messageType), 0, frame)
}

// Ping sends an empty PING. It doesn't wait for the PONG.
func (c *Conn) Ping() error {
	c.writeMu.Lock()
	err := c.writeFrameLocked(opPing, 0, c.controlWrite[:MaxHeaderSize])
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
	budget := c.drainBudget() - c.message.remaining
	if c.message.remaining < 0 || budget < 0 {
		return
	}
	if c.discard(c.message.remaining) != nil {
		return
	}
	for {
		hdr, err := readHeader(c.reader)
		if err != nil || hdr.check(c.client, c.deflate) != nil || hdr.opcode == opClose {
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

// failMessage fails the connection for an error found in a message's
// payload.
func (c *Conn) failMessage(err error) error {
	switch {
	case errors.Is(err, ErrInvalidUTF8):
		return c.fail(StatusInvalidPayloadData, ErrInvalidUTF8)
	case errors.Is(err, ErrMessageTooBig):
		return c.fail(StatusMessageTooBig, err)
	default:
		// Chromium also treats data that fails to inflate as a protocol error.
		return c.fail(StatusProtocolError, fmt.Errorf("%w: inflate: %w", ErrProtocol, err))
	}
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
	_ = c.writeFrameLocked(opPong, 0, frame)
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
	c.message.remaining = 0
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
	err := c.writeFrameLocked(opClose, 0, frame)
	c.closeSent.Store(true)
	return err == nil, err
}

// writeFrameLocked sends a final frame with one Write. rsv holds the
// reserved bits to set. The caller holds writeMu.
func (c *Conn) writeFrameLocked(frameOpcode opcode, rsv byte, frame []byte) error {
	if c.closeSent.Load() || c.closed.Load() {
		return fmt.Errorf("websocket: write: %w", net.ErrClosed)
	}
	payload := frame[MaxHeaderSize:]
	start := putHeader(frame[:MaxHeaderSize], frameOpcode, rsv, len(payload), c.client)
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

// writeMessage sends a data frame under writeMu.
func (c *Conn) writeMessage(frameOpcode opcode, rsv byte, frame []byte) error {
	c.writeMu.Lock()
	err := c.writeFrameLocked(frameOpcode, rsv, frame)
	c.unlockWrite()
	return err
}

func newConn(transport io.ReadWriteCloser, reader *bufio.Reader, client bool) *Conn {
	conn := &Conn{transport: transport, reader: reader, client: client}
	conn.message.conn = conn
	conn.message.reader = reader
	return conn
}

func newDeflateConn(transport io.ReadWriteCloser, reader *bufio.Reader, client, deflate bool, opts Options) *Conn {
	conn := newConn(transport, reader, client)
	conn.deflate = deflate
	conn.threshold = opts.compressionThreshold()
	return conn
}
