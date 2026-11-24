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
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// Conn is a WebSocket connection. Read and Reader must not be called
// concurrently with each other. Every other method may be.
//
// Control frames are handled inside Read and Reader, so a connection must be
// read for Ping and Close to see the peer's replies. A read or write whose
// context ends closes the connection.
type Conn struct {
	transport   io.ReadWriteCloser
	reader      *bufio.Reader
	writer      *bufio.Writer
	client      bool
	subprotocol string
	onPing      func(context.Context, []byte) bool
	abort       func()

	readLimit atomic.Int64
	readMu    lock
	// Guarded by readMu.
	readErr error
	message messageReader
	control [maxControlPayload]byte

	writeMu lock
	// Guarded by writeMu.
	headerBuf [maxHeaderSize]byte

	stateMu       sync.Mutex
	closeSent     bool
	closeReceived error

	closing   atomic.Bool
	closed    chan struct{}
	closeOnce sync.Once

	pingCount atomic.Int64
	pingsMu   sync.Mutex
	pings     map[string]chan<- struct{}
}

// Close sends a Close frame with code and reason, waits for the peer's Close
// frame, and closes the connection. Each wait is bounded at 5s. Later calls
// return an error wrapping [net.ErrClosed].
func (c *Conn) Close(code StatusCode, reason string) error {
	if c.closing.Swap(true) {
		<-c.closed
		return fmt.Errorf("close: %w", net.ErrClosed)
	}
	err := c.closeHandshake(code, reason)
	if closeErr := c.closeTransport(); err == nil {
		err = closeErr
	}
	if err == nil || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("close: %w", err)
}

// CloseNow closes the connection without a close handshake. It ends a Close
// that is waiting on the peer.
func (c *Conn) CloseNow() error {
	alreadyClosing := c.closing.Swap(true)
	err := c.closeTransport()
	switch {
	case alreadyClosing:
		return fmt.Errorf("close: %w", net.ErrClosed)
	case err != nil && !errors.Is(err, net.ErrClosed):
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// Ping sends a ping and waits for the matching pong, which a concurrent read
// receives. A context that ends while waiting doesn't close the connection.
func (c *Conn) Ping(ctx context.Context) error {
	payload := strconv.FormatInt(c.pingCount.Add(1), 10)
	pong := make(chan struct{}, 1)
	c.pingsMu.Lock()
	c.pings[payload] = pong
	c.pingsMu.Unlock()
	defer func() {
		c.pingsMu.Lock()
		delete(c.pings, payload)
		c.pingsMu.Unlock()
	}()
	if err := c.writeControl(ctx, opPing, []byte(payload)); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	select {
	case <-pong:
		return nil
	case <-c.closed:
		return fmt.Errorf("ping: %w", net.ErrClosed)
	case <-ctx.Done():
		return fmt.Errorf("wait for pong: %w", ctx.Err())
	}
}

// Read reads a whole message.
func (c *Conn) Read(ctx context.Context) (MessageType, []byte, error) {
	messageType, reader, err := c.Reader(ctx)
	if err != nil {
		return 0, nil, err
	}
	data, err := io.ReadAll(reader)
	return messageType, data, err
}

// Reader waits for the next message and returns a reader for it, bounded by
// ctx. The message must be read to EOF before the next call.
func (c *Conn) Reader(ctx context.Context) (MessageType, io.Reader, error) {
	if err := c.readMu.acquire(ctx, c.closed); err != nil {
		return 0, nil, fmt.Errorf("get reader: %w", err)
	}
	defer c.readMu.release()
	if c.readErr != nil {
		return 0, nil, c.readErr
	}
	if c.message.active {
		return 0, nil, errors.New("previous message not read to completion")
	}
	hdr, err := c.nextFrame(ctx)
	if err != nil {
		return 0, nil, err
	}
	if hdr.opcode == opContinuation {
		c.message.frameLeft = hdr.length
		return 0, nil, c.fail(ctx, StatusProtocolError, errors.New("continuation frame without a message"))
	}
	c.message.start(ctx, hdr.opcode == opText)
	if err := c.startFrame(ctx, hdr); err != nil {
		return 0, nil, err
	}
	return MessageType(hdr.opcode), &c.message, nil
}

// SetReadLimit sets the largest message a read accepts. A larger message
// closes the connection with [StatusMessageTooBig]. The default is 32768
// bytes and -1 removes the limit.
func (c *Conn) SetReadLimit(limit int64) {
	c.readLimit.Store(max(limit, -1))
}

// Subprotocol returns the negotiated subprotocol, or "" for none.
func (c *Conn) Subprotocol() string {
	return c.subprotocol
}

// Write sends a message in one frame.
func (c *Conn) Write(ctx context.Context, messageType MessageType, data []byte) error {
	if messageType != MessageText && messageType != MessageBinary {
		return fmt.Errorf("write: invalid message type %d", messageType)
	}
	if err := c.writeFrame(ctx, opcode(messageType), data); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	return nil
}

// closeHandshake bounds each step itself, since Close takes no context, as
// in coder/websocket. contextcheck reads the directive as an opt-out for
// callers, which nolintlint doesn't see.
//
//nolint:contextcheck,nolintlint // see above
func (c *Conn) closeHandshake(code StatusCode, reason string) error {
	ctx := context.Background()
	if err := c.writeClose(ctx, code, reason); err != nil {
		return err
	}
	err := c.waitCloseHandshake(ctx)
	if CloseStatus(err) == code {
		return nil
	}
	return err
}

// closeTransport returns [net.ErrClosed] after the first call.
func (c *Conn) closeTransport() error {
	err := net.ErrClosed
	c.closeOnce.Do(func() {
		close(c.closed)
		err = c.transport.Close()
	})
	return err
}

// fail sends a Close frame for a protocol violation and makes reads return
// the error. Close later drains the peer's frames.
func (c *Conn) fail(ctx context.Context, code StatusCode, cause error) error {
	_ = c.writeClose(ctx, code, closeReason(cause.Error()))
	c.readErr = &failError{cause: cause}
	return c.readErr
}

// failClose fails the connection for an invalid Close frame and closes the
// transport. The peer won't send another Close, leaving nothing to wait for.
func (c *Conn) failClose(ctx context.Context, code StatusCode, cause error) error {
	err := c.fail(ctx, code, cause)
	c.stateMu.Lock()
	c.closeReceived = err
	c.stateMu.Unlock()
	if !c.closing.Swap(true) {
		_ = c.closeTransport()
	}
	return err
}

func (c *Conn) handleControl(ctx context.Context, hdr header) error {
	payload := c.control[:hdr.length]
	stop := c.watch(ctx)
	_, err := io.ReadFull(c.reader, payload)
	stop()
	if err != nil {
		return c.readFailed(ctx, err)
	}
	if hdr.masked {
		mask(payload, hdr.maskKey, 0)
	}
	switch hdr.opcode { //nolint:exhaustive // check admits only control opcodes
	case opPing:
		if c.onPing != nil && !c.onPing(ctx, payload) {
			return nil
		}
		if err := c.writeControl(ctx, opPong, payload); err != nil && !errors.Is(err, net.ErrClosed) {
			return err
		}
	case opPong:
		c.pingsMu.Lock()
		pong, ok := c.pings[string(payload)]
		c.pingsMu.Unlock()
		if ok {
			select {
			case pong <- struct{}{}:
			default:
			}
		}
	default:
		return c.receiveClose(ctx, payload)
	}
	return nil
}

func (c *Conn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// nextFrame reads frames until a data frame, handling control frames on the
// way. The caller holds readMu.
func (c *Conn) nextFrame(ctx context.Context) (header, error) {
	for {
		c.stateMu.Lock()
		received := c.closeReceived
		c.stateMu.Unlock()
		if received != nil {
			return header{}, received
		}
		stop := c.watch(ctx)
		hdr, err := readHeader(c.reader)
		stop()
		if errors.Is(err, errFrameLength) {
			return hdr, c.fail(ctx, StatusProtocolError, err)
		} else if err != nil {
			return hdr, c.readFailed(ctx, err)
		}
		if err := hdr.check(c.client); err != nil {
			if hdr.opcode == opClose {
				return hdr, c.failClose(ctx, StatusProtocolError, err)
			}
			c.message.frameLeft = hdr.length
			return hdr, c.fail(ctx, StatusProtocolError, err)
		}
		if !hdr.opcode.isControl() {
			return hdr, nil
		}
		if err := c.handleControl(ctx, hdr); err != nil {
			return hdr, err
		}
	}
}

// readFailed reports a read error from the transport, preferring the reason
// the transport closed.
func (c *Conn) readFailed(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		err = ctx.Err()
	case c.isClosed():
		err = net.ErrClosed
	}
	c.readErr = fmt.Errorf("read: %w", err)
	return c.readErr
}

func (c *Conn) readPayload(ctx context.Context, data []byte) (int, error) {
	stop := c.watch(ctx)
	size, err := c.reader.Read(data)
	stop()
	if err != nil {
		return size, c.readFailed(ctx, err)
	}
	return size, nil
}

// receiveClose echoes the peer's Close frame and closes the transport, unless
// a Close call is waiting to do it.
func (c *Conn) receiveClose(ctx context.Context, payload []byte) error {
	closeErr, code, err := parseClose(payload)
	if err != nil {
		return c.failClose(ctx, code, err)
	}
	received := fmt.Errorf("received close frame: %w", closeErr)
	c.stateMu.Lock()
	c.closeReceived = received
	sent := c.closeSent
	c.stateMu.Unlock()
	if !sent {
		_ = c.writeClose(ctx, closeErr.Code, closeErr.Reason)
	}
	if !c.closing.Swap(true) {
		_ = c.closeTransport()
	}
	return received
}

// startFrame begins reading a data frame's payload, enforcing the read limit
// from its header.
func (c *Conn) startFrame(ctx context.Context, hdr header) error {
	message := &c.message
	message.fin = hdr.fin
	message.frameLeft = hdr.length
	message.maskKey = hdr.maskKey
	message.maskPos = 0
	if limit := c.readLimit.Load(); limit >= 0 && hdr.length > limit-message.total {
		cause := fmt.Errorf("%w: read limited at %d bytes", ErrMessageTooBig, limit)
		return c.fail(ctx, StatusMessageTooBig, cause)
	}
	message.total += hdr.length
	return nil
}

// waitCloseHandshake discards data until the peer's Close frame arrives.
func (c *Conn) waitCloseHandshake(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, closeTimeout)
	defer cancel()
	if err := c.readMu.acquire(ctx, c.closed); err != nil {
		return err
	}
	defer c.readMu.release()
	// A read of a message cut short here must fail rather than end in EOF.
	if c.readErr == nil && c.message.active && (c.message.frameLeft > 0 || !c.message.fin) {
		c.readErr = fmt.Errorf("read: %w", net.ErrClosed)
	}
	for {
		stop := c.watch(ctx)
		_, err := io.CopyN(io.Discard, c.reader, c.message.frameLeft)
		stop()
		c.message.frameLeft = 0
		if err != nil {
			return c.readFailed(ctx, err)
		}
		hdr, err := c.nextFrame(ctx)
		if err != nil {
			return err
		}
		c.message.frameLeft = hdr.length
	}
}

// watch closes the transport when ctx ends, which unblocks pending I/O.
func (c *Conn) watch(ctx context.Context) func() bool {
	if ctx.Done() == nil {
		return noStop
	}
	return context.AfterFunc(ctx, c.abort)
}

// writeClose isn't bounded by ctx, which may have ended a read.
func (c *Conn) writeClose(ctx context.Context, code StatusCode, reason string) error {
	var payload []byte
	if code != StatusNoStatusRcvd {
		if !validCloseCode(code) {
			return fmt.Errorf("status code %d can't be sent", code)
		}
		if len(reason) > maxCloseReason {
			return fmt.Errorf("close reason of %d bytes exceeds %d", len(reason), maxCloseReason)
		}
		payload = binary.BigEndian.AppendUint16(make([]byte, 0, 2+len(reason)), uint16(code)) //nolint:gosec // validCloseCode bounds code
		payload = append(payload, reason...)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	// The frame may be lost to a concurrent close, which the handshake
	// tolerates.
	if err := c.writeFrame(ctx, opClose, payload); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

func (c *Conn) writeControl(ctx context.Context, frameOpcode opcode, payload []byte) error {
	ctx, cancel := context.WithTimeout(ctx, closeTimeout)
	defer cancel()
	return c.writeFrame(ctx, frameOpcode, payload)
}

func (c *Conn) writeFrame(ctx context.Context, frameOpcode opcode, payload []byte) error {
	if err := c.writeMu.acquire(ctx, c.closed); err != nil {
		return err
	}
	defer c.writeMu.release()
	c.stateMu.Lock()
	sent := c.closeSent
	c.stateMu.Unlock()
	if sent {
		return fmt.Errorf("close frame already sent: %w", net.ErrClosed)
	}
	stop := c.watch(ctx)
	err := c.writeFrameLocked(frameOpcode, payload)
	stop()
	if err != nil {
		switch {
		case ctx.Err() != nil:
			err = ctx.Err()
		case c.isClosed():
			err = net.ErrClosed
		}
		// A partial frame leaves the stream unusable.
		_ = c.closeTransport()
		return fmt.Errorf("write frame: %w", err)
	}
	if frameOpcode == opClose {
		c.stateMu.Lock()
		c.closeSent = true
		received := c.closeReceived != nil
		c.stateMu.Unlock()
		if received && !c.closing.Swap(true) {
			_ = c.closeTransport()
		}
	}
	return nil
}

// writeFrameLocked masks a client's payload as it copies it into the write
// buffer, leaving the caller's slice untouched.
func (c *Conn) writeFrameLocked(frameOpcode opcode, payload []byte) error {
	var maskKey *[4]byte
	var key [4]byte
	if c.client {
		_, _ = rand.Read(key[:]) // never fails, see crypto/rand.Read
		maskKey = &key
	}
	if _, err := c.writer.Write(appendHeader(c.headerBuf[:0], frameOpcode, len(payload), maskKey)); err != nil {
		return err
	}
	if !c.client {
		if _, err := c.writer.Write(payload); err != nil {
			return err
		}
		return c.writer.Flush()
	}
	pos := 0
	for len(payload) > 0 {
		if c.writer.Available() == 0 {
			if err := c.writer.Flush(); err != nil {
				return err
			}
		}
		chunk := c.writer.AvailableBuffer()
		size := min(len(payload), cap(chunk))
		chunk = append(chunk, payload[:size]...)
		pos = mask(chunk, key, pos)
		if _, err := c.writer.Write(chunk); err != nil {
			return err
		}
		payload = payload[size:]
	}
	return c.writer.Flush()
}

// failError is a read error that failed the connection. It doesn't wrap the
// CloseError sent, which CloseStatus would mistake for the peer's.
type failError struct {
	cause error
}

func (e *failError) Error() string {
	return "failed the connection: " + e.cause.Error()
}

func (e *failError) Unwrap() error {
	return e.cause
}

// lock is a mutex whose acquisition gives up when ctx ends or the connection
// closes.
type lock chan struct{}

func (l lock) acquire(ctx context.Context, closed <-chan struct{}) error {
	select {
	case <-closed:
		return net.ErrClosed
	case <-ctx.Done():
		return fmt.Errorf("acquire lock: %w", ctx.Err())
	case l <- struct{}{}:
	}
	// The send may win over a closed connection.
	select {
	case <-closed:
		l.release()
		return net.ErrClosed
	default:
		return nil
	}
}

func (l lock) release() {
	<-l
}

// messageReader reads the message Reader returned. Its fields are guarded by
// the connection's readMu.
type messageReader struct {
	conn *Conn
	ctx  context.Context //nolint:containedctx // Reader's ctx bounds its message, as in coder/websocket

	active    bool
	text      bool
	fin       bool
	total     int64
	frameLeft int64
	maskKey   [4]byte
	maskPos   int
	utf8      utf8Validator
}

func (m *messageReader) Read(data []byte) (int, error) {
	conn := m.conn
	if err := conn.readMu.acquire(m.ctx, conn.closed); err != nil {
		return 0, fmt.Errorf("read: %w", err)
	}
	defer conn.readMu.release()
	if conn.readErr != nil {
		return 0, conn.readErr
	}
	if !m.active {
		return 0, io.EOF
	}
	for m.frameLeft == 0 {
		if m.fin {
			m.active = false
			if m.text && !m.utf8.complete() {
				return 0, conn.fail(m.ctx, StatusInvalidFramePayloadData, fmt.Errorf("%w: message ends inside a sequence", ErrInvalidUTF8))
			}
			return 0, io.EOF
		}
		hdr, err := conn.nextFrame(m.ctx)
		if err != nil {
			return 0, err
		}
		if hdr.opcode != opContinuation {
			m.frameLeft = hdr.length
			return 0, conn.fail(m.ctx, StatusProtocolError, errors.New("data frame inside a fragmented message"))
		}
		if err := conn.startFrame(m.ctx, hdr); err != nil {
			return 0, err
		}
	}
	if int64(len(data)) > m.frameLeft {
		data = data[:m.frameLeft]
	}
	size, err := conn.readPayload(m.ctx, data)
	data = data[:size]
	m.frameLeft -= int64(size)
	if !conn.client {
		m.maskPos = mask(data, m.maskKey, m.maskPos)
	}
	if m.text && !m.utf8.valid(data) {
		return size, conn.fail(m.ctx, StatusInvalidFramePayloadData, ErrInvalidUTF8)
	}
	return size, err
}

func (m *messageReader) start(ctx context.Context, text bool) {
	m.ctx = ctx
	m.active = true
	m.text = text
	m.total = 0
	m.utf8 = utf8Validator{}
}

func newConn(transport io.ReadWriteCloser, reader *bufio.Reader, writer *bufio.Writer, client bool) *Conn {
	conn := &Conn{
		transport: transport,
		reader:    reader,
		writer:    writer,
		client:    client,
		readMu:    make(lock, 1),
		writeMu:   make(lock, 1),
		closed:    make(chan struct{}),
		pings:     make(map[string]chan<- struct{}),
	}
	conn.message.conn = conn
	conn.abort = func() { _ = conn.closeTransport() }
	conn.readLimit.Store(defaultReadLimit)
	return conn
}

// parseClose parses a Close frame's payload. On failure it returns the code
// to fail the connection with.
func parseClose(payload []byte) (CloseError, StatusCode, error) {
	if len(payload) == 0 {
		return CloseError{Code: StatusNoStatusRcvd}, 0, nil
	}
	if len(payload) == 1 {
		return CloseError{}, StatusProtocolError, errors.New("close frame payload of 1 byte")
	}
	closeErr := CloseError{
		Code:   StatusCode(binary.BigEndian.Uint16(payload)),
		Reason: string(payload[2:]),
	}
	if !validCloseCode(closeErr.Code) {
		return CloseError{}, StatusProtocolError, fmt.Errorf("close frame has invalid status code %d", closeErr.Code)
	}
	if !utf8.ValidString(closeErr.Reason) {
		return CloseError{}, StatusInvalidFramePayloadData, errors.New("close reason is not valid UTF-8")
	}
	return closeErr, 0, nil
}

func noStop() bool {
	return false
}
