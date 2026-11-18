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

const (
	// flagEnvelopeCompressed indicates that the data is compressed. It has the
	// same meaning in the gRPC-Web, gRPC-HTTP2, and Connect protocols.
	flagEnvelopeCompressed = 0b00000001

	// envelopePrefixSize is the size of the prefix before each message in gRPC
	// and Connect streams. The first byte is a set of bitwise flags, and the
	// rest is the message length as a big-endian uint32. gRPC and Connect
	// interpret the flags differently, so the caller interprets them.
	envelopePrefixSize = 5
)

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
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	padEnvelope(buffer)
	if err := w.codec.MarshalWrite(w.ctx, buffer, message); err != nil {
		return connect.Errorf(connect.CodeInternal, "marshal message: %s", err).WithCause(err)
	}
	return w.Write(0, buffer)
}

// Write compresses the message in buffer as necessary, then sends it with
// the given flags. The buffer must start with the padding from padEnvelope.
// Write doesn't retain the buffer.
func (w *envelopeWriter) Write(flags uint8, buffer *bytes.Buffer) *connect.Error {
	size := buffer.Len() - envelopePrefixSize
	compressedSize := 0
	if w.compressionPool != nil && size >= w.compressMinBytes {
		compressed := bufferpool.Get()
		defer bufferpool.Put(compressed)
		padEnvelope(compressed)
		buffer.Next(envelopePrefixSize) // compress only the message
		if err := w.compressionPool.Compress(compressed, buffer); err != nil {
			return err
		}
		buffer = compressed
		compressedSize = buffer.Len() - envelopePrefixSize
		flags |= flagEnvelopeCompressed
		if w.sendMaxBytes > 0 && compressedSize > w.sendMaxBytes {
			return connect.Errorf(connect.CodeResourceExhausted, "compressed message size %d exceeds sendMaxBytes %d", compressedSize, w.sendMaxBytes)
		}
	} else if w.sendMaxBytes > 0 && size > w.sendMaxBytes {
		return connect.Errorf(connect.CodeResourceExhausted, "message size %d exceeds sendMaxBytes %d", size, w.sendMaxBytes)
	}
	if err := w.write(flags, buffer); err != nil {
		return err
	}
	// Protocol messages, like the end of stream, aren't counted.
	if w.stats != nil && flags&^flagEnvelopeCompressed == 0 {
		*w.stats = connect.MessageStats{Size: size, CompressedSize: compressedSize}
	}
	return nil
}

// write fills in the prefix of buffer and sends it, without compressing it or
// checking its size. The buffer must start with the padding from padEnvelope.
func (w *envelopeWriter) write(flags uint8, buffer *bytes.Buffer) *connect.Error {
	data := buffer.Bytes()
	// Cast as 64-bit to ensure comparison works on all architectures.
	size := int64(len(data) - envelopePrefixSize)
	if size > math.MaxUint32 {
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
	ctx             context.Context //nolint:containedctx
	reader          io.Reader
	limited         io.LimitedReader // avoids allocating one per message
	bytesRead       int64            // detect trailers-only gRPC responses
	codec           connect.Codec
	compressionPool *compressionPool
	readMaxBytes    int
	stats           *connect.MessageStats
}

// Read reads the next enveloped message into a pooled buffer, decompressing
// it, and returns the buffer and the envelope's flags. The caller must return
// the buffer to the pool, even on error.
//
// A message with protocol-specific flags ends the stream. Read checks that no
// data follows it, and leaves the message for the caller to interpret.
func (r *envelopeReader) Read() (*bytes.Buffer, uint8, *connect.Error) {
	buffer := bufferpool.Get()
	// Read the prefix into the buffer's spare capacity. It isn't kept: the
	// message overwrites it.
	buffer.Grow(envelopePrefixSize)
	prefix := buffer.AvailableBuffer()[:envelopePrefixSize]
	// io.ReadFull reads the number of bytes requested, or returns an error.
	// io.EOF will only be returned if no bytes were read.
	n, err := io.ReadFull(r.reader, prefix)
	r.bytesRead += int64(n)
	if err != nil {
		if errors.Is(err, io.EOF) {
			// The stream ended cleanly. That's expected, but we need to propagate an EOF
			// to the user so that they know that the stream has ended. We shouldn't
			// add any alarming text about protocol errors, though.
			return buffer, 0, connect.Errorf(connect.CodeUnknown, "%s", err).WithCause(err)
		}
		err = wrapIfMaxBytesError(err, "read 5 byte message prefix")
		err = wrapIfContextDone(r.ctx, err)
		if connectErr, ok := asError(err); ok {
			return buffer, 0, connectErr
		}
		// Something else has gone wrong - the stream didn't end cleanly.
		return buffer, 0, connect.Errorf(
			connect.CodeInternal,
			"protocol error: incomplete envelope: %s", err,
		).WithCause(err)
	}
	flags := prefix[0]
	size := int64(binary.BigEndian.Uint32(prefix[1:envelopePrefixSize]))
	if r.readMaxBytes > 0 && size > int64(r.readMaxBytes) {
		n, err := io.CopyN(io.Discard, r.reader, size)
		r.bytesRead += n
		if err != nil && !errors.Is(err, io.EOF) {
			return buffer, 0, connect.Errorf(connect.CodeResourceExhausted, "message is larger than configured max %d - unable to determine message size: %s", r.readMaxBytes, err).WithCause(err)
		}
		return buffer, 0, connect.Errorf(connect.CodeResourceExhausted, "message size %d is larger than configured max %d", size, r.readMaxBytes)
	}
	// The buffer grows as bytes arrive since size comes from the peer.
	r.limited = io.LimitedReader{R: r.reader, N: size}
	readN, err := buffer.ReadFrom(&r.limited)
	r.bytesRead += readN
	if readN == size {
		err = nil // like io.CopyN, ignore an error returned with the last bytes
	} else if err == nil {
		err = io.EOF // ReadFrom hides EOF, so a short read needs it put back
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			// We've gotten fewer bytes than we expected, so the stream has ended
			// unexpectedly.
			return buffer, 0, connect.Errorf(connect.CodeInternal,
				"protocol error: promised %d bytes in enveloped message, got %d bytes",
				size,
				readN,
			).WithCause(io.ErrUnexpectedEOF)
		}
		err = wrapIfMaxBytesError(err, "read %d byte message", size)
		err = wrapIfContextDone(r.ctx, err)
		if connectErr, ok := asError(err); ok {
			return buffer, 0, connectErr
		}
		return buffer, 0, connect.Errorf(connect.CodeUnknown, "read enveloped message: %s", err).WithCause(err)
	}

	compressedSize := 0
	if flags&flagEnvelopeCompressed != 0 {
		if r.compressionPool == nil {
			return buffer, 0, connect.Errorf(connect.CodeInternal,
				"protocol error: sent compressed message without compression support",
			)
		}
		if buffer.Len() > 0 {
			compressedSize = buffer.Len()
			decompressed := bufferpool.Get()
			err := r.compressionPool.Decompress(decompressed, buffer, int64(r.readMaxBytes))
			bufferpool.Put(buffer)
			buffer = decompressed
			if err != nil {
				return buffer, 0, err
			}
		}
	}

	if flags&^flagEnvelopeCompressed == 0 {
		if r.stats != nil {
			*r.stats = connect.MessageStats{Size: buffer.Len(), CompressedSize: compressedSize}
		}
		return buffer, flags, nil
	}
	// Drain the rest of the stream to ensure there is no extra data.
	numBytes, err := discard(r.reader)
	r.bytesRead += numBytes
	if err != nil {
		err = wrapIfContextError(err)
		if connErr, ok := asError(err); ok && connErr.Code() != connect.CodeUnavailable {
			return buffer, 0, connErr
		}
		// Ignore uncoded read errors and unavailable transport errors if the
		// complete end-stream message was already received. In HTTP/1.1,
		// http.Transport closes the connection on request write failures,
		// which can cause subsequent reads to fail despite having received
		// the entire response.
	}
	if numBytes > 0 {
		return buffer, 0, connect.Errorf(connect.CodeInternal, "corrupt response: %d extra bytes after end of stream", numBytes)
	}
	return buffer, flags, nil
}

// unmarshal decodes a message from a buffer returned by Read.
func (r *envelopeReader) unmarshal(buffer *bytes.Buffer, message any) *connect.Error {
	if buffer.Len() == 0 {
		// There's no data, so the zero value of the message is correct.
		return nil
	}
	if err := r.codec.UnmarshalRead(r.ctx, buffer, message); err != nil {
		return connect.Errorf(connect.CodeInvalidArgument, "unmarshal message: %s", err).WithCause(err)
	}
	return nil
}

// padEnvelope writes zeros to reserve the envelope prefix at the front of
// buffer. Write the message after it. envelopeWriter fills in the prefix when
// sending.
func padEnvelope(buffer *bytes.Buffer) {
	var prefix [envelopePrefixSize]byte
	_, _ = buffer.Write(prefix[:])
}
