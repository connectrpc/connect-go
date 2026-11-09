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
	"io"
	"math"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/bufferpool"
)

// flagEnvelopeCompressed indicates that the data is compressed. It has the
// same meaning in the gRPC-Web, gRPC-HTTP2, and Connect protocols.
const flagEnvelopeCompressed = 0b00000001

// envelopePrefixSize is the size of the prefix that precedes each message in
// gRPC and Connect's framing protocol. The first byte is a uint8 used as a
// set of bitwise flags, and the remainder is a big-endian uint32 holding the
// message length. gRPC and Connect interpret the bitwise flags differently,
// so the envelope code leaves their interpretation up to the caller.
const envelopePrefixSize = 5

var errSpecialEnvelope = connect.Errorf(
	connect.CodeUnknown,
	"final message has protocol-specific flags: %s",
	io.EOF,
).WithCause(io.EOF) // User code checks for end of stream with errors.Is(err, io.EOF).

type envelopeWriter struct {
	ctx              context.Context //nolint:containedctx
	sender           messageSender
	codec            connect.Codec
	compressMinBytes int
	compressionPool  *compressionPool
	sendMaxBytes     int
	stats            *connect.MessageStats
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
		// Send no data to create the request and send headers.
		if err := w.sender.Send(nil); err != nil {
			if connectErr, ok := asError(err); ok {
				return connectErr
			}
			return connect.Errorf(connect.CodeUnknown, "%s", err).WithCause(err)
		}
		return nil
	}
	frame := newEnvelopeBuffer()
	defer bufferpool.Put(frame)
	if err := w.codec.MarshalWrite(w.ctx, frame, message); err != nil {
		return connect.Errorf(connect.CodeInternal, "marshal message: %s", err).WithCause(err)
	}
	return w.Write(0, frame)
}

// Write sends the message in a buffer from newEnvelopeBuffer, compressing as
// necessary. It doesn't retain any references to the buffer.
func (w *envelopeWriter) Write(flags uint8, frame *bytes.Buffer) *connect.Error {
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
		if err := w.write(flags, frame); err != nil {
			return err
		}
		w.recordStats(flags, size, compressedSize)
		return nil
	}
	compressed := newEnvelopeBuffer()
	defer bufferpool.Put(compressed)
	frame.Next(envelopePrefixSize) // Compress reads only the message.
	if err := w.compressionPool.Compress(compressed, frame); err != nil {
		return err
	}
	compressedSize := compressed.Len() - envelopePrefixSize
	if w.sendMaxBytes > 0 && compressedSize > w.sendMaxBytes {
		return connect.Errorf(connect.CodeResourceExhausted, "compressed message size %d exceeds sendMaxBytes %d", compressedSize, w.sendMaxBytes)
	}
	if err := w.write(flags|flagEnvelopeCompressed, compressed); err != nil {
		return err
	}
	w.recordStats(flags, size, compressedSize)
	return nil
}

// write fills in the prefix of a buffer from newEnvelopeBuffer and sends it
// with one write, without compressing or checking its size.
func (w *envelopeWriter) write(flags uint8, frame *bytes.Buffer) *connect.Error {
	data := frame.Bytes()
	// Cast as 64-bit to ensure comparison works on all architectures.
	size := int64(len(data) - envelopePrefixSize)
	if size < 0 || size > math.MaxUint32 {
		return connect.Errorf(connect.CodeInternal, "envelope size %d out of bounds", size)
	}
	data[0] = flags
	binary.BigEndian.PutUint32(data[1:envelopePrefixSize], uint32(size))
	if err := w.sender.Send(data); err != nil {
		err = wrapIfContextDone(w.ctx, err)
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		return connect.Errorf(connect.CodeUnknown, "write envelope: %s", err).WithCause(err)
	}
	return nil
}

type envelopeReader struct {
	ctx       context.Context //nolint:containedctx
	reader    io.Reader
	prefix    [envelopePrefixSize]byte
	limited   io.LimitedReader
	bytesRead int64 // detect trailers-only gRPC responses
	codec     connect.Codec
	// lastFlags and lastData hold the end of stream message, which has
	// protocol-specific flags.
	lastFlags       uint8
	lastData        *bytes.Buffer
	compressionPool *compressionPool
	readMaxBytes    int
	stats           *connect.MessageStats
}

func (r *envelopeReader) Unmarshal(message any) error {
	buffer := bufferpool.Get()
	var dontRelease *bytes.Buffer
	defer func() {
		if buffer != dontRelease {
			bufferpool.Put(buffer)
		}
	}()

	flags, err := r.Read(buffer)
	switch {
	case err == nil && flags&flagEnvelopeCompressed != 0 && r.compressionPool == nil:
		return connect.Errorf(connect.CodeInternal,
			"protocol error: sent compressed message without compression support",
		)
	case err == nil &&
		(flags == 0 || flags == flagEnvelopeCompressed) &&
		buffer.Len() == 0:
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

	data := buffer
	compressedSize := 0
	if data.Len() > 0 && flags&flagEnvelopeCompressed != 0 {
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

	if flags != 0 && flags != flagEnvelopeCompressed {
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
		r.lastFlags, r.lastData = flags, data
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

// Read reads one enveloped message into data, without its prefix, and returns
// the envelope's flags.
func (r *envelopeReader) Read(data *bytes.Buffer) (uint8, error) {
	// io.ReadFull reads the number of bytes requested, or returns an error.
	// io.EOF will only be returned if no bytes were read.
	n, err := io.ReadFull(r.reader, r.prefix[:])
	r.bytesRead += int64(n)
	if err != nil {
		if errors.Is(err, io.EOF) {
			// The stream ended cleanly. That's expected, but we need to propagate an EOF
			// to the user so that they know that the stream has ended. We shouldn't
			// add any alarming text about protocol errors, though.
			return 0, io.EOF // bare so the end of every stream doesn't allocate
		}
		err = wrapIfMaxBytesError(err, "read 5 byte message prefix")
		err = wrapIfContextDone(r.ctx, err)
		if connectErr, ok := asError(err); ok {
			return 0, connectErr
		}
		// Something else has gone wrong - the stream didn't end cleanly.
		return 0, connect.Errorf(
			connect.CodeInternal,
			"protocol error: incomplete envelope: %s", err,
		).WithCause(err)
	}
	size := int64(binary.BigEndian.Uint32(r.prefix[1:5]))
	if r.readMaxBytes > 0 && size > int64(r.readMaxBytes) {
		n, err := io.CopyN(io.Discard, r.reader, size)
		r.bytesRead += n
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, connect.Errorf(connect.CodeResourceExhausted, "message is larger than configured max %d - unable to determine message size: %s", r.readMaxBytes, err).WithCause(err)
		}
		return 0, connect.Errorf(connect.CodeResourceExhausted, "message size %d is larger than configured max %d", size, r.readMaxBytes)
	}
	// We've read the prefix, so we know how many bytes to expect. Like
	// io.CopyN, but without allocating a limited reader per message. The buffer
	// grows only as bytes arrive, since the size comes from the peer.
	r.limited = io.LimitedReader{R: r.reader, N: size}
	readN, err := data.ReadFrom(&r.limited)
	if err == nil && readN < size {
		err = io.EOF
	}
	r.bytesRead += readN
	if err != nil {
		if errors.Is(err, io.EOF) {
			// We've gotten fewer bytes than we expected, so the stream has ended
			// unexpectedly.
			return 0, connect.Errorf(connect.CodeInternal,
				"protocol error: promised %d bytes in enveloped message, got %d bytes",
				size,
				readN,
			).WithCause(io.ErrUnexpectedEOF)
		}
		err = wrapIfMaxBytesError(err, "read %d byte message", size)
		err = wrapIfContextDone(r.ctx, err)
		if connectErr, ok := asError(err); ok {
			return 0, connectErr
		}
		return 0, connect.Errorf(connect.CodeUnknown, "read enveloped message: %s", err).WithCause(err)
	}
	return r.prefix[0], nil
}

// newEnvelopeBuffer returns a pooled buffer holding room for the envelope
// prefix. Write the message after it, then pass the buffer to
// envelopeWriter.Write, which fills in the prefix. The caller must return the
// buffer to the pool.
func newEnvelopeBuffer() *bytes.Buffer {
	frame := bufferpool.Get()
	resetEnvelopeBuffer(frame)
	return frame
}

// resetEnvelopeBuffer empties the buffer, leaving room for the envelope
// prefix. Writing may drain the buffer, so reset it before writing again.
func resetEnvelopeBuffer(frame *bytes.Buffer) {
	frame.Reset()
	var prefix [envelopePrefixSize]byte
	_, _ = frame.Write(prefix[:])
}
