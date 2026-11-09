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

package connecthttp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/bufferpool"
)

// flagEnvelopeCompressed indicates that the data is compressed. It has the
// same meaning in the gRPC-Web, gRPC-HTTP2, and Connect protocols.
const flagEnvelopeCompressed = 0b00000001

// envelopePrefixSize is the size of the flags and message length that precede
// each message.
const envelopePrefixSize = 5

var errSpecialEnvelope = connect.Errorf(
	connect.CodeUnknown,
	"final message has protocol-specific flags: %s",
	io.EOF,
).WithCause(io.EOF) // User code checks for end of stream with errors.Is(err, io.EOF).

// envelope is a block of arbitrary bytes wrapped in gRPC and Connect's framing
// protocol.
//
// Each message is preceded by a 5-byte prefix. The first byte is a uint8 used
// as a set of bitwise flags, and the remainder is a uint32 indicating the
// message length. gRPC and Connect interpret the bitwise flags differently, so
// envelope leaves their interpretation up to the caller. Data holds the
// message without the prefix.
type envelope struct {
	Data  *bytes.Buffer
	Flags uint8
}

func (e *envelope) IsSet(flag uint8) bool {
	return e.Flags&flag == flag
}

type envelopeWriter struct {
	ctx              context.Context //nolint:containedctx
	sender           messageSender
	codec            connect.Codec
	compressMinBytes int
	compressionPool  *compressionPool
	sendMaxBytes     int
	stats            *connect.MessageStats
	// frame is the payload handed to the sender, reused to avoid allocating
	// one per message.
	frame bytes.Reader
}

// recordStats records message byte counts, skipping protocol envelopes.
func (w *envelopeWriter) recordStats(flags uint8, size, compressedSize int) {
	if w.stats == nil || flags&^flagEnvelopeCompressed != 0 {
		return
	}
	*w.stats = connect.MessageStats{Size: size, CompressedSize: compressedSize}
}

func (w *envelopeWriter) Marshal(message any) *connect.Error {
	if message == nil {
		// Send no-op message to create the request and send headers.
		payload := nopPayload{}
		if _, err := w.sender.Send(payload); err != nil {
			if connectErr, ok := asError(err); ok {
				return connectErr
			}
			return connect.Errorf(connect.CodeUnknown, "%s", err).WithCause(err)
		}
		return nil
	}
	// Marshal after room for the prefix so the message is sent with one write.
	frame := newFrameBuffer()
	defer bufferpool.Put(frame)
	if err := w.codec.MarshalWrite(w.ctx, frame, message); err != nil {
		return connect.Errorf(connect.CodeInternal, "marshal message: %s", err).WithCause(err)
	}
	return w.writeFrame(0, frame)
}

// Write writes the enveloped message, compressing as necessary. It doesn't
// retain any references to the supplied envelope or its underlying data.
func (w *envelopeWriter) Write(env *envelope) *connect.Error {
	frame := frameEnvelope(env)
	defer bufferpool.Put(frame)
	return w.writeFrame(env.Flags, frame)
}

// writeFrame writes a message from a buffer holding room for the prefix
// followed by the message, compressing as necessary.
func (w *envelopeWriter) writeFrame(flags uint8, frame *bytes.Buffer) *connect.Error {
	size := frame.Len() - envelopePrefixSize
	isCompressed := flags&flagEnvelopeCompressed == flagEnvelopeCompressed
	if isCompressed || w.compressionPool == nil || size < w.compressMinBytes {
		if w.sendMaxBytes > 0 && size > w.sendMaxBytes {
			return connect.Errorf(connect.CodeResourceExhausted, "message size %d exceeds sendMaxBytes %d", size, w.sendMaxBytes)
		}
		compressedSize := 0
		if isCompressed {
			// Pre-compressed payload; uncompressed size unknown.
			size, compressedSize = 0, size
		}
		if err := w.sendFrame(flags, frame); err != nil {
			return err
		}
		w.recordStats(flags, size, compressedSize)
		return nil
	}
	compressed := newFrameBuffer()
	defer bufferpool.Put(compressed)
	frame.Next(envelopePrefixSize) // Compress reads only the message.
	if err := w.compressionPool.Compress(compressed, frame); err != nil {
		return err
	}
	compressedSize := compressed.Len() - envelopePrefixSize
	if w.sendMaxBytes > 0 && compressedSize > w.sendMaxBytes {
		return connect.Errorf(connect.CodeResourceExhausted, "compressed message size %d exceeds sendMaxBytes %d", compressedSize, w.sendMaxBytes)
	}
	if err := w.sendFrame(flags|flagEnvelopeCompressed, compressed); err != nil {
		return err
	}
	w.recordStats(flags, size, compressedSize)
	return nil
}

// write sends the envelope as is, without compressing or checking its size.
func (w *envelopeWriter) write(env *envelope) *connect.Error {
	frame := frameEnvelope(env)
	defer bufferpool.Put(frame)
	return w.sendFrame(env.Flags, frame)
}

// sendFrame fills in the prefix of a buffer holding room for the prefix
// followed by the message, and sends it with a single write. The buffer must
// not be reused until sendFrame returns.
func (w *envelopeWriter) sendFrame(flags uint8, frame *bytes.Buffer) *connect.Error {
	data := frame.Bytes()
	prefix, err := makeEnvelopePrefix(flags, len(data)-envelopePrefixSize)
	if err != nil {
		return connect.Errorf(connect.CodeInternal, "%s", err).WithCause(err)
	}
	copy(data, prefix[:])
	w.frame.Reset(data)
	if _, err := w.sender.Send(&w.frame); err != nil {
		err = wrapIfContextDone(w.ctx, err)
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		return connect.Errorf(connect.CodeUnknown, "write envelope: %s", err).WithCause(err)
	}
	return nil
}

type envelopeReader struct {
	ctx             context.Context //nolint:containedctx
	reader          io.Reader
	prefix          [envelopePrefixSize]byte
	limited         io.LimitedReader
	bytesRead       int64 // detect trailers-only gRPC responses
	codec           connect.Codec
	last            envelope
	compressionPool *compressionPool
	readMaxBytes    int
	stats           *connect.MessageStats
}

func (r *envelopeReader) Unmarshal(message any) *connect.Error {
	buffer := bufferpool.Get()
	var dontRelease *bytes.Buffer
	defer func() {
		if buffer != dontRelease {
			bufferpool.Put(buffer)
		}
	}()

	env := &envelope{Data: buffer}
	err := r.Read(env)
	switch {
	case err == nil && env.IsSet(flagEnvelopeCompressed) && r.compressionPool == nil:
		return connect.Errorf(connect.CodeInternal,
			"protocol error: sent compressed message without compression support",
		)
	case err == nil &&
		(env.Flags == 0 || env.Flags == flagEnvelopeCompressed) &&
		env.Data.Len() == 0:
		// This is a standard message (because none of the top 7 bits are set) and
		// there's no data, so the zero value of the message is correct.
		if r.stats != nil {
			*r.stats = connect.MessageStats{}
		}
		return nil
	case err != nil && errors.Is(err, io.EOF):
		// The stream has ended. Propagate the EOF to the caller.
		return err
	case err != nil:
		// Something's wrong.
		return err
	}

	data := env.Data
	compressedSize := 0
	if data.Len() > 0 && env.IsSet(flagEnvelopeCompressed) {
		compressedSize = data.Len()
		decompressed := bufferpool.Get()
		defer func() {
			if decompressed != dontRelease {
				bufferpool.Put(decompressed)
			}
		}()
		if err := r.compressionPool.Decompress(decompressed, data, int64(r.readMaxBytes)); err != nil {
			return err
		}
		data = decompressed
	}

	if env.Flags != 0 && env.Flags != flagEnvelopeCompressed {
		// Drain the rest of the stream to ensure there is no extra data.
		numBytes, err := discard(r.reader)
		r.bytesRead += numBytes
		if err != nil {
			err = wrapIfContextError(err)
			if connErr, ok := asError(err); ok && connErr.Code() != connect.CodeUnavailable {
				return connErr
			}
			// Ignore uncoded read errors and unavailable transport errors if the
			// complete end-stream message was already received. In HTTP/1.1,
			// http.Transport closes the connection on request write failures,
			// which can cause subsequent reads to fail despite having received
			// the entire response.
		}
		if numBytes > 0 {
			return connect.Errorf(connect.CodeInternal, "corrupt response: %d extra bytes after end of stream", numBytes)
		}
		// One of the protocol-specific flags are set, so this is the end of the
		// stream. Save the message for protocol-specific code to process and
		// return a sentinel error. We alias the buffer with dontRelease as a
		// way of marking it so above defers don't release it to the pool.
		r.last = envelope{
			Data:  data,
			Flags: env.Flags,
		}
		dontRelease = data
		return errSpecialEnvelope
	}

	size := data.Len() // before UnmarshalRead drains the buffer
	if err := r.codec.UnmarshalRead(r.ctx, data, message); err != nil {
		return connect.Errorf(connect.CodeInvalidArgument, "unmarshal message: %s", err).WithCause(err)
	}
	if r.stats != nil {
		*r.stats = connect.MessageStats{Size: size, CompressedSize: compressedSize}
	}
	return nil
}

func (r *envelopeReader) Read(env *envelope) *connect.Error {
	// io.ReadFull reads the number of bytes requested, or returns an error.
	// io.EOF will only be returned if no bytes were read.
	n, err := io.ReadFull(r.reader, r.prefix[:])
	r.bytesRead += int64(n)
	if err != nil {
		if errors.Is(err, io.EOF) {
			// The stream ended cleanly. That's expected, but we need to propagate an EOF
			// to the user so that they know that the stream has ended. We shouldn't
			// add any alarming text about protocol errors, though.
			return connect.Errorf(connect.CodeUnknown, "%s", err).WithCause(err)
		}
		err = wrapIfMaxBytesError(err, "read 5 byte message prefix")
		err = wrapIfContextDone(r.ctx, err)
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		// Something else has gone wrong - the stream didn't end cleanly.
		return connect.Errorf(
			connect.CodeInternal,
			"protocol error: incomplete envelope: %s", err,
		).WithCause(err)
	}
	size := int64(binary.BigEndian.Uint32(r.prefix[1:5]))
	if r.readMaxBytes > 0 && size > int64(r.readMaxBytes) {
		n, err := io.CopyN(io.Discard, r.reader, size)
		r.bytesRead += n
		if err != nil && !errors.Is(err, io.EOF) {
			return connect.Errorf(connect.CodeResourceExhausted, "message is larger than configured max %d - unable to determine message size: %s", r.readMaxBytes, err).WithCause(err)
		}
		return connect.Errorf(connect.CodeResourceExhausted, "message size %d is larger than configured max %d", size, r.readMaxBytes)
	}
	// We've read the prefix, so we know how many bytes to expect. Like
	// io.CopyN, but without allocating a limited reader per message.
	r.limited = io.LimitedReader{R: r.reader, N: size}
	readN, err := env.Data.ReadFrom(&r.limited)
	if err == nil && readN < size {
		err = io.EOF
	}
	r.bytesRead += readN
	if err != nil {
		if errors.Is(err, io.EOF) {
			// We've gotten fewer bytes than we expected, so the stream has ended
			// unexpectedly.
			return connect.Errorf(connect.CodeInternal,
				"protocol error: promised %d bytes in enveloped message, got %d bytes",
				size,
				readN,
			).WithCause(io.ErrUnexpectedEOF)
		}
		err = wrapIfMaxBytesError(err, "read %d byte message", size)
		err = wrapIfContextDone(r.ctx, err)
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		return connect.Errorf(connect.CodeUnknown, "read enveloped message: %s", err).WithCause(err)
	}
	env.Flags = r.prefix[0]
	return nil
}

// newFrameBuffer returns a pooled buffer holding room for the prefix. The
// prefix is filled in once the message size is known. The caller must return
// the buffer to the pool.
func newFrameBuffer() *bytes.Buffer {
	frame := bufferpool.Get()
	var prefix [envelopePrefixSize]byte
	_, _ = frame.Write(prefix[:])
	return frame
}

// frameEnvelope copies the envelope into a pooled buffer with room for the
// prefix at the front. The caller must return the buffer to the pool.
func frameEnvelope(env *envelope) *bytes.Buffer {
	frame := newFrameBuffer()
	_, _ = frame.Write(env.Data.Bytes())
	return frame
}

func makeEnvelopePrefix(flags uint8, size int) ([envelopePrefixSize]byte, error) {
	// Cast as 64-bit to ensure comparison works on all architectures.
	size64 := int64(size)
	if size64 < 0 || size64 > math.MaxUint32 {
		return [envelopePrefixSize]byte{}, fmt.Errorf("connect.makeEnvelopePrefix: size %d out of bounds", size)
	}
	prefix := [envelopePrefixSize]byte{}
	prefix[0] = flags
	binary.BigEndian.PutUint32(prefix[1:5], uint32(size64))
	return prefix, nil
}
