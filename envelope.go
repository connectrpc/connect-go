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
)

// flagEnvelopeCompressed indicates that the data is compressed. It has the
// same meaning in the gRPC-Web, gRPC-HTTP2, and Connect protocols.
const flagEnvelopeCompressed = 0b00000001

// envelope is a block of arbitrary bytes wrapped in gRPC and Connect's framing
// protocol.
//
// Each message is preceded by a 5-byte prefix. The first byte is a uint8 used
// as a set of bitwise flags, and the remainder is a uint32 indicating the
// message length. gRPC and Connect interpret the bitwise flags differently, so
// envelope leaves their interpretation up to the caller.
type envelope struct {
	Data  *bytes.Buffer
	Flags uint8
}

func (e envelope) WriteTo(w io.Writer) (n int64, err error) {
	prefix := [5]byte{}
	prefix[0] = e.Flags
	binary.BigEndian.PutUint32(prefix[1:5], uint32(e.Data.Len()))
	for _, b := range [2][]byte{prefix[:], e.Data.Bytes()} {
		wroteN, err := w.Write(b)
		if err != nil {
			if writeErr, ok := asError(err); ok {
				return n, writeErr
			}
			return n, errorf(CodeUnknown, "write envelope: %w", err)
		}
		n += int64(wroteN)
	}
	return n, nil
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

func read(dst *bytes.Buffer, src io.Reader) (int, error) {
	dst.Grow(bytes.MinRead)
	b := dst.Bytes()
	b = b[len(b):cap(b)]
	n, err := src.Read(b)
	_, _ = dst.Write(b[:n])
	return n, err
}

func readAll(dst *bytes.Buffer, src io.Reader, readMaxBytes int) *Error {
	var totalN int64
	for {
		readN, err := read(dst, src)
		totalN += int64(readN)
		if readMaxBytes > 0 && totalN > int64(readMaxBytes) {
			discardN, err := discard(src)
			if err != nil {
				return errorf(CodeResourceExhausted,
					"message is larger than configured max %d - unable to determine message size: %w",
					readMaxBytes, err)
			}
			return errorf(CodeResourceExhausted,
				"message size %d is larger than configured max %d",
				totalN+discardN, readMaxBytes)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if writeErr, ok := asError(err); ok {
				return writeErr
			}
			if readMaxBytesErr := asMaxBytesError(err, "read first %d bytes of message", totalN); readMaxBytesErr != nil {
				return readMaxBytesErr
			}
			return errorf(CodeUnknown, "read: %w", err)
		}
	}
}

var errEOF = errorf(CodeInternal, "%w", io.EOF)

func readEnvelope(dst *bytes.Buffer, src io.Reader, readMaxBytes int) (uint8, *Error) {
	prefix := [5]byte{}
	if _, err := io.ReadFull(src, prefix[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, errEOF
		}
		// Something else has gone wrong - the stream didn't end cleanly.
		if connectErr, ok := asError(err); ok {
			return 0, connectErr
		}
		if maxBytesErr := asMaxBytesError(err, "read 5 byte message prefix"); maxBytesErr != nil {
			// We're reading from an http.MaxBytesHandler, and we've exceeded the read limit.
			return 0, maxBytesErr
		}
		return 0, errorf(CodeInternal, "incomplete envelope: %w", err)
	}

	size := int(binary.BigEndian.Uint32(prefix[1:5]))
	if size < 0 {
		return 0, errorf(CodeInvalidArgument, "message size %d overflowed uint32", size)
	}
	if readMaxBytes > 0 && size > readMaxBytes {
		if _, err := discard(src); err != nil {
			return 0, errorf(CodeUnknown, "read enveloped message: %w", err)
		}
		return 0, errorf(CodeResourceExhausted, "message size %d is larger than configured max %d", size, readMaxBytes)
	}
	if size > 0 {
		dst.Grow(size)
		data := dst.Bytes()[dst.Len() : dst.Len()+size]
		if _, err := io.ReadFull(src, data); err != nil {
			if maxBytesErr := asMaxBytesError(err, "read %d byte message", size); maxBytesErr != nil {
				// We're reading from an http.MaxBytesHandler, and we've exceeded the read limit.
				return 0, maxBytesErr
			}
			return 0, errorf(CodeInternal, "incomplete envelope: %w", err)
		}
		if _, err := dst.Write(data); err != nil {
			return 0, errorf(CodeInternal, "read enveloped message: %w", err)
		}
	}
	return prefix[0], nil
}
func writeAll(dst io.Writer, src io.WriterTo) *Error {
	if _, err := src.WriteTo(dst); err != nil {
		if writeErr, ok := asError(err); ok {
			return writeErr
		}
		return errorf(CodeInternal, "write message: %w", err)
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

func newErrInvalidEnvelopeFlags(flags uint8) *Error {
	return errorf(CodeInternal, "protocol error: invalid envelope flags %08b", flags)
}

// ensureEOF always returns io.EOF, unless there are extra bytes in src.
func ensureEOF(src io.Reader) error {
	if n, err := discard(src); err != nil {
		return err
	} else if n > 0 {
		return errorf(CodeInternal, "corrupt response: %d extra bytes after end of stream", n)
	}
	return io.EOF
}
