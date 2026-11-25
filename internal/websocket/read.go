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
	"fmt"
	"io"
	"net"
)

// messageReader reads a data message's payload across its frames, unmasking
// it. It handles control frames between fragments, and validates plain text
// as it goes. For a compressed message it implements io.ByteReader for flate
// and appends deflateTail after the last frame.
type messageReader struct {
	conn      *Conn
	reader    *bufio.Reader
	remaining int64 // payload bytes left in the current frame
	fin       bool  // the current frame is the message's last
	masked    bool
	maskKey   [4]byte
	maskPos   int
	// total counts the payload so far, and budget bounds it. A negative
	// budget means no bound.
	total      int64
	budget     int64
	limit      int64
	compressed bool
	dropped    bool   // the message arrived after this side's Close frame
	tail       string // what's left of deflateTail
	text       bool
	utf8       utf8Validator
}

// Read returns io.EOF with the message's last bytes, which ends
// bytes.Buffer.ReadFrom without another grow.
func (m *messageReader) Read(data []byte) (int, error) {
	for m.remaining == 0 {
		if m.fin {
			m.startTail()
			size := copy(data, m.tail)
			if m.tail = m.tail[size:]; m.tail == "" {
				return size, io.EOF
			}
			return size, nil
		}
		if err := m.next(); err != nil {
			return 0, err
		}
	}
	if int64(len(data)) > m.remaining {
		data = data[:m.remaining]
	}
	size, err := m.reader.Read(data)
	if size == 0 && err != nil {
		return 0, m.conn.readFailed(err)
	}
	data = data[:size]
	m.remaining -= int64(size)
	if m.masked {
		m.maskPos = mask(data, m.maskKey, m.maskPos)
	}
	if m.text && !m.utf8.valid(data) {
		return size, ErrInvalidUTF8
	}
	if m.remaining == 0 && m.fin && m.tail == "" {
		return size, io.EOF
	}
	return size, nil
}

// ReadByte serves flate's bit reader.
func (m *messageReader) ReadByte() (byte, error) {
	for m.remaining == 0 {
		if m.fin {
			m.startTail()
			if m.tail == "" {
				return 0, io.EOF
			}
			next := m.tail[0]
			m.tail = m.tail[1:]
			return next, nil
		}
		if err := m.next(); err != nil {
			return 0, err
		}
	}
	next, err := m.reader.ReadByte()
	if err != nil {
		return 0, m.conn.readFailed(err)
	}
	m.remaining--
	if m.masked {
		next ^= m.maskKey[m.maskPos]
		m.maskPos = (m.maskPos + 1) & 3
	}
	return next, nil
}

// begin starts a message. text validates the payload itself as UTF-8.
func (m *messageReader) begin(text, compressed bool, limit int64) {
	m.total = 0
	m.limit = limit
	m.budget = limit
	if compressed {
		m.budget = compressedLimit(limit)
	}
	m.compressed = compressed
	m.dropped = false
	m.tail = ""
	m.text = text
	m.utf8 = utf8Validator{}
}

// charge counts size bytes against the message's bound and reports whether
// they fit.
func (m *messageReader) charge(size int64) bool {
	if m.budget >= 0 && size > m.budget-m.total {
		return false
	}
	m.total += size
	return true
}

// finished reports whether the whole message and its tail were read.
func (m *messageReader) finished() bool {
	return m.fin && m.remaining == 0 && m.tail == ""
}

// morePayload reports whether payload bytes are left, skipping empty frames.
func (m *messageReader) morePayload() (bool, error) {
	for m.remaining == 0 && !m.fin {
		if err := m.next(); err != nil {
			return false, err
		}
	}
	return m.remaining > 0, nil
}

// next moves to the message's next frame.
func (m *messageReader) next() error {
	if err := m.conn.nextFrame(); err != nil {
		return err
	}
	if m.dropped {
		return errDropped
	}
	return nil
}

// startTail drops the sync tail after an empty payload. No encoder sends
// one, since an empty message deflates to 0x00, but zlib and other
// implementations read it as an empty message.
func (m *messageReader) startTail() {
	if m.total == 0 && m.tail == deflateTail {
		m.tail = deflateTail[len(syncTail):]
	}
}

func (m *messageReader) tooBig() error {
	if m.compressed {
		return fmt.Errorf("%w: compressed size is over %d bytes for a limit of %d", ErrMessageTooBig, m.budget, m.limit)
	}
	return fmt.Errorf("%w: limit is %d bytes", ErrMessageTooBig, m.limit)
}

// readDataHeader reads frame headers until a data frame's, handling control
// frames on the way. inMessage says whether a fragmented message is open.
// The caller holds readMu.
func (c *Conn) readDataHeader(inMessage bool) (header, error) {
	for {
		hdr, err := readHeader(c.reader)
		if err != nil {
			return hdr, c.readFailed(err)
		}
		c.message.remaining = hdr.length
		if err := hdr.check(c.client, c.deflate); err != nil {
			err = fmt.Errorf("%w: %w", ErrProtocol, err)
			if hdr.opcode == opClose {
				// The peer has closed, which leaves nothing to drain.
				_, _ = c.writeClose(StatusProtocolError, "")
				_ = c.Close()
				c.readErr = err
				return hdr, err
			}
			return hdr, c.fail(StatusProtocolError, err)
		}
		switch {
		case hdr.opcode.isControl():
			if err := c.readControl(hdr); err != nil {
				return hdr, err
			}
			continue
		case hdr.opcode == opContinuation && !inMessage:
			return hdr, c.fail(StatusProtocolError, fmt.Errorf("%w: continuation frame without a message", ErrProtocol))
		case hdr.opcode != opContinuation && inMessage:
			return hdr, c.fail(StatusProtocolError, fmt.Errorf("%w: data frame inside a fragmented message", ErrProtocol))
		}
		return hdr, nil
	}
}

// startFrame sets up the message reader for a data frame's payload. It
// applies the message's bound before reading any of it, and after this
// side's Close frame it counts the frame against the drain budget instead.
func (c *Conn) startFrame(hdr header) error {
	message := &c.message
	if c.closeSent.Load() {
		message.dropped = true
		if c.discarded += hdr.length; c.discarded > c.drainBudget() {
			_ = c.Close()
			c.readErr = fmt.Errorf("websocket: peer sent over %d bytes after Close: %w", c.drainBudget(), net.ErrClosed)
			return c.readErr
		}
	} else if !message.charge(hdr.length) {
		return c.fail(StatusMessageTooBig, message.tooBig())
	}
	message.remaining = hdr.length
	message.fin = hdr.fin
	message.masked = hdr.masked
	message.maskKey = hdr.maskKey
	message.maskPos = 0
	return nil
}

// nextFrame moves the message reader to the message's next frame.
func (c *Conn) nextFrame() error {
	hdr, err := c.readDataHeader(true)
	if err != nil {
		return err
	}
	return c.startFrame(hdr)
}

// skipMessage discards the rest of the message.
func (c *Conn) skipMessage() error {
	for {
		if err := c.discard(c.message.remaining); err != nil {
			return c.readFailed(err)
		}
		c.message.remaining = 0
		if c.message.fin {
			return nil
		}
		if err := c.nextFrame(); err != nil {
			return err
		}
	}
}

// readPlain appends an uncompressed message to dst.
func (c *Conn) readPlain(dst *bytes.Buffer) error {
	// The extra MinRead covers ReadFrom's first grow, which would otherwise
	// reallocate a buffer grown to exactly the frame's length.
	dst.Grow(int(min(c.message.remaining, maxGrow)) + bytes.MinRead)
	if _, err := dst.ReadFrom(&c.message); err != nil {
		return err
	}
	if c.message.text && !c.message.utf8.complete() {
		return ErrInvalidUTF8
	}
	return nil
}

// inflateMessage appends a compressed message's inflated bytes to dst,
// reading the payload as it goes.
func (c *Conn) inflateMessage(dst *bytes.Buffer, text bool, limit int64) error {
	c.message.tail = deflateTail
	reader := getFlateReader(&c.message)
	c.inflate = inflater{
		source: &c.message,
		reader: reader,
		dst:    dst,
		start:  dst.Len(),
		left:   limit,
		text:   text,
	}
	_, err := dst.ReadFrom(&c.inflate)
	if err == nil && text && !c.inflate.utf8.complete() {
		err = ErrInvalidUTF8
	}
	c.inflate = inflater{}
	flateReaderPool.Put(reader)
	return err
}
