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
	Buffer *bytes.Buffer
}

// makeEnvelope creates an envelope with a 5-byte prefix.
func makeEnvelope(buffer *bytes.Buffer) envelope {
	var head [5]byte
	buffer.Write(head[:])
	return envelope{Buffer: buffer}
}

// encodeSizeAndFlags writes the size and flags to the envelope's prefix.
func (e envelope) encodeSizeAndFlags(flags uint8) {
	e.Buffer.Bytes()[0] = flags
	binary.BigEndian.PutUint32(e.Buffer.Bytes()[1:5], uint32(e.size()))
}

// size returns the size of the envelope's payload, in bytes.
func (e envelope) size() int {
	return e.Buffer.Len() - 5
}

// unwrap returns the underlying buffer, skipping the prefix.
func (e envelope) unwrap() *bytes.Buffer {
	e.Buffer.Next(5) // skip the prefix
	return e.Buffer
}

func marshal(dst *bytes.Buffer, message any, codec Codec) *Error {
	if message == nil {
		return nil
	}
	if codec, ok := codec.(marshalAppender); ok {
		// Codec supports MarshalAppend; try to re-use a []byte from the pool.
		raw, err := codec.MarshalAppend(dst.Bytes(), message)
		if err != nil {
			return errorf(CodeInternal, "marshal message: %w", err)
		}
		if cap(raw) > dst.Cap() {
			// The buffer from the pool was too small, so MarshalAppend grew the slice.
			// Pessimistically assume that the too-small buffer is insufficient for the
			// application workload, so there's no point in keeping it in the pool.
			// Instead, replace it with the larger, newly-allocated slice. This
			// allocates, but it's a small, constant-size allocation.
			*dst = *bytes.NewBuffer(raw)
		} else {
			// The buffer from the pool was large enough, MarshalAppend didn't allocate.
			// Copy to the same byte slice is a nop.
			dst.Write(raw[dst.Len():])
		}
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
