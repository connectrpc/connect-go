// Copyright 2021-2023 Buf Technologies, Inc.
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

package connect

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

// flagEnvelopeCompressed indicates that the data is compressed. It has the
// same meaning in the gRPC-Web, gRPC-HTTP2, and Connect protocols.
const flagEnvelopeCompressed = 0b00000001

func newErrInvalidEnvelopeFlags(flags uint8) *Error {
	return errorf(CodeInternal, "protocol error: invalid envelope flags %08b", flags)
}

// envelope is a block of arbitrary bytes wrapped in gRPC and Connect's framing
// protocol.
//
// Each message is preceded by a 5-byte prefix. The first byte is a uint8 used
// as a set of bitwise flags, and the remainder is a uint32 indicating the
// message length. gRPC and Connect interpret the bitwise flags differently, so
// envelope leaves their interpretation up to the caller.
type envelope struct {
	*bytes.Buffer
}

func makeEnvelope(buffer *bytes.Buffer) envelope {
	buffer.Reset() // Reset the buffer to the beginning.
	var head [5]byte
	_, _ = buffer.Write(head[:])
	return envelope{buffer}
}
func (e envelope) flags() uint8 {
	return e.Bytes()[0]
}
func (e envelope) encodeSizeAndFlags(flags uint8) {
	e.Bytes()[0] = flags
	binary.BigEndian.PutUint32(e.Bytes()[1:5], uint32(e.Len()-5))
}
func (e envelope) decodeSize() int {
	return int(binary.BigEndian.Uint32(e.Bytes()[1:5]))
}
func (e envelope) size() int {
	return e.Len() - 5
}

type messageParams struct {
	codec            Codec            // Codec to use for marshalling.
	buffer           *bytes.Buffer    // Buffer to use for marshalling.
	compressBuffer   *bytes.Buffer    // Buffer to use for compression.
	bufferPool       *bufferPool      // Buffer Pool to use for allocating buffers.
	compressionPool  *compressionPool // CompressionPool to use for compressing buffers.
	maxBytes         int              // Maximum size of the message.
	compressMinBytes int              // Minimum size of the message to compress.
}

func (p *messageParams) getBuffer() *bytes.Buffer {
	if p.buffer == nil {
		p.buffer = p.bufferPool.Get()
	}
	p.buffer.Reset()
	return p.buffer
}
func (p *messageParams) getCompressionBuffer() *bytes.Buffer {
	if p.compressBuffer == nil {
		p.compressBuffer = p.bufferPool.Get()
	}
	p.compressBuffer.Reset()
	return p.compressBuffer
}
func (p *messageParams) drop() {
	if p.buffer != nil {
		p.bufferPool.Put(p.buffer)
		p.buffer = nil
	}
	if p.compressBuffer != nil {
		p.bufferPool.Put(p.compressBuffer)
		p.compressBuffer = nil
	}
}

func marshal(dst *bytes.Buffer, message any, codec Codec) *Error {
	if message == nil {
		return nil
	}
	if codec, ok := codec.(marshalAppender); ok {
		// Codec supports MarshalAppend; try to re-use a []byte from the pool.
		raw, err := codec.MarshalAppend(dst.Bytes()[dst.Len():], message)
		if err != nil {
			return errorf(CodeInternal, "marshal message: %w", err)
		}
		dst.Write(raw)
		return nil
	}
	// Codec doesn't support MarshalAppend; let Marshal allocate a []byte.
	raw, err := codec.Marshal(message)
	if err != nil {
		return errorf(CodeInternal, "marshal message: %w", err)
	}
	dst.Write(raw)
	return nil
}

func unmarshal(src *bytes.Buffer, message any, codec Codec) *Error {
	if err := codec.Unmarshal(src.Bytes(), message); err != nil {
		return errorf(CodeInvalidArgument, "unmarshal into %T: %w", message, err)
	}
	return nil
}

func compress(buffer *bytes.Buffer, bufferPool *bufferPool, compressionPool *compressionPool) *Error {
	data := bufferPool.Get()
	defer bufferPool.Put(data)
	if err := compressionPool.Compress(data, buffer); err != nil {
		return err
	}
	buffer.Reset()
	_, _ = data.WriteTo(buffer)
	return nil
}
func decompress(buffer *bytes.Buffer, bufferPool *bufferPool, compressionPool *compressionPool, readMaxBytes int) *Error {
	data := bufferPool.Get()
	defer bufferPool.Put(data)
	if err := compressionPool.Decompress(data, buffer, int64(readMaxBytes)); err != nil {
		return err
	}
	buffer.Reset()
	_, _ = data.WriteTo(buffer)
	return nil
}

// func envelopeBuffer(flags uint8, src *bytes.Buffer) {
// 	prefix := [5]byte{}
// 	prefix[0] = flags
// 	binary.BigEndian.PutUint32(prefix[1:5], uint32(src.Len()))
// 	buf := append(src.Bytes(), prefix[:]...)
// 	copy(buf[5:], buf[:5])
// 	copy(buf[:5], prefix[:])
// 	if cap(buf) > src.Cap() {
// 		*src = *bytes.NewBuffer(buf)
// 	} else {
// 		src.Reset()
// 		src.Write(buf)
// 	}
// }

func writeEnvelope(dst io.Writer, flags uint8, src *bytes.Buffer) *Error {
	prefix := [5]byte{}
	prefix[0] = flags
	binary.BigEndian.PutUint32(prefix[1:5], uint32(src.Len()))
	if _, err := dst.Write(prefix[:]); err != nil {
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		return errorf(CodeUnknown, "write envelope: %w", err)
	}
	if _, err := src.WriteTo(dst); err != nil {
		return errorf(CodeUnknown, "write message: %w", err)
	}
	return nil
}

func readEnvelope(dst *bytes.Buffer, src io.Reader, readMaxBytes int) (uint8, *Error) {
	prefixes := [5]byte{}
	prefixBytesRead, err := src.Read(prefixes[:])

	switch {
	case (err == nil || errors.Is(err, io.EOF)) &&
		prefixBytesRead == 5 &&
		isSizeZeroPrefix(prefixes):
		// Successfully read prefix and expect no additional data.
		return prefixes[0], nil
	case err != nil && errors.Is(err, io.EOF) && prefixBytesRead == 0:
		// The stream ended cleanly. That's expected, but we need to propagate them
		// to the user so that they know that the stream has ended. We shouldn't
		// add any alarming text about protocol errors, though.
		return 0, NewError(CodeUnknown, err)
	case err != nil || prefixBytesRead < 5:
		// Something else has gone wrong - the stream didn't end cleanly.
		if connectErr, ok := asError(err); ok {
			return 0, connectErr
		}
		if maxBytesErr := asMaxBytesError(err, "read 5 byte message prefix"); maxBytesErr != nil {
			// We're reading from an http.MaxBytesHandler, and we've exceeded the read limit.
			return 0, maxBytesErr
		}
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return 0, errorf(
			CodeInvalidArgument,
			"protocol error: incomplete envelope: %w", err,
		)
	}
	size := int(binary.BigEndian.Uint32(prefixes[1:5]))
	if size < 0 {
		return 0, errorf(CodeInvalidArgument, "message size %d overflowed uint32", size)
	}
	if readMaxBytes > 0 && size > readMaxBytes {
		_, err := io.CopyN(io.Discard, src, int64(size))
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, errorf(CodeUnknown, "read enveloped message: %w", err)
		}
		return 0, errorf(CodeResourceExhausted, "message size %d is larger than configured max %d", size, readMaxBytes)
	}
	if size > 0 {
		dst.Grow(size)
		// At layer 7, we don't know exactly what's happening down in L4. Large
		// length-prefixed messages may arrive in chunks, so we may need to read
		// the request body past EOF. We also need to take care that we don't retry
		// forever if the message is malformed.
		remaining := int64(size)
		for remaining > 0 {
			bytesRead, err := io.CopyN(dst, src, remaining)
			if err != nil && !errors.Is(err, io.EOF) {
				if maxBytesErr := asMaxBytesError(err, "read %d byte message", size); maxBytesErr != nil {
					// We're reading from an http.MaxBytesHandler, and we've exceeded the read limit.
					return 0, maxBytesErr
				}
				return 0, errorf(CodeUnknown, "read enveloped message: %w", err)
			}
			if errors.Is(err, io.EOF) && bytesRead == 0 {
				// We've gotten zero-length chunk of data. Message is likely malformed,
				// don't wait for additional chunks.
				return 0, errorf(
					CodeInvalidArgument,
					"protocol error: promised %d bytes in enveloped message, got %d bytes",
					size,
					int64(size)-remaining,
				)
			}
			remaining -= bytesRead
		}
	}
	return prefixes[0], nil
}

func isSizeZeroPrefix(prefix [5]byte) bool {
	return prefix[1]|prefix[2]|prefix[3]|prefix[4] == 0
}

func readAll(dst *bytes.Buffer, src io.Reader, readMaxBytes int) *Error {
	limitReader := src
	if readMaxBytes > 0 && int64(readMaxBytes) < math.MaxInt64 {
		limitReader = io.LimitReader(src, int64(readMaxBytes)+1)
	}

	// ReadFrom ignores io.EOF, so any error here is real.
	bytesRead, err := dst.ReadFrom(limitReader)
	if err != nil {
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		if readMaxBytesErr := asMaxBytesError(err, "read first %d bytes of message", bytesRead); readMaxBytesErr != nil {
			return readMaxBytesErr
		}
		return errorf(CodeUnknown, "read message: %w", err)
	}
	if readMaxBytes > 0 && bytesRead > int64(readMaxBytes) {
		// Attempt to read to end in order to allow connection re-use
		discardedBytes, err := io.Copy(io.Discard, src)
		if err != nil {
			return errorf(CodeResourceExhausted,
				"message is larger than configured max %d - unable to determine message size: %w",
				readMaxBytes, err)
		}
		return errorf(CodeResourceExhausted,
			"message size %d is larger than configured max %d",
			bytesRead+discardedBytes, readMaxBytes)
	}
	return nil
}

func checkSendMaxBytes(length, sendMaxBytes int, isCompressed bool) *Error {
	if sendMaxBytes <= 0 || length <= sendMaxBytes {
		return nil
	}
	tmpl := "message size %d exceeds sendMaxBytes %d"
	if isCompressed {
		tmpl = "compressed message size %d exceeds sendMaxBytes %d"
	}
	return errorf(CodeResourceExhausted, tmpl, length, sendMaxBytes)
}

func writeAll(dst io.Writer, src *bytes.Buffer) *Error {
	if _, err := src.WriteTo(dst); err != nil {
		if writeErr, ok := asError(err); ok {
			return writeErr
		}
		return errorf(CodeUnknown, "write message: %w", err)
	}
	return nil
}
