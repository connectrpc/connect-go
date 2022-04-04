// Copyright 2021-2022 Buf Technologies, Inc.
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
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/textproto"
)

const (
	flagLPMCompressed = 0b00000001
	flagLPMTrailer    = 0b10000000
)

var errGotWebTrailers = errorf(
	CodeUnknown,
	"end of message stream, next block of data is gRPC-Web trailers: %w",
	// User code checks for end of stream with errors.Is(err, io.EOF).
	io.EOF,
)

type marshaler struct {
	writer           io.Writer
	compressionPool  compressionPool
	codec            Codec
	compressMinBytes int
}

func (m *marshaler) Marshal(message any) (sizes, *Error) {
	raw, err := m.codec.Marshal(message)
	if err != nil {
		return sizes{}, errorf(CodeInternal, "couldn't marshal message: %w", err)
	}
	return m.writeLPM(false /* trailer */, raw)
}

func (m *marshaler) MarshalWebTrailers(trailer http.Header) *Error {
	// OPT: easy opportunity to pool buffers
	raw := bytes.NewBuffer(nil)
	if err := trailer.Write(raw); err != nil {
		return errorf(CodeInternal, "couldn't format trailers: %w", err)
	}
	_, err := m.writeLPM(true /* trailer */, raw.Bytes())
	return err
}

func (m *marshaler) writeLPM(trailer bool, message []byte) (sizes, *Error) {
	if m.compressionPool == nil || len(message) < m.compressMinBytes {
		if err := m.writeGRPCPrefix(false /* compressed */, trailer, len(message)); err != nil {
			return sizes{}, err // already enriched
		}
		if _, err := m.writer.Write(message); err != nil {
			return sizes{}, errorf(
				CodeUnknown,
				"couldn't write message of length-prefixed message: %w",
				err,
			)
		}
		return sizes{
			Uncompressed: uint(len(message)),
			Wire:         uint(len(message)),
		}, nil
	}
	// OPT: easy opportunity to pool buffers
	data := bytes.NewBuffer(make([]byte, 0, len(message)))
	compressor, err := m.compressionPool.GetWriter(data)
	if err != nil {
		return sizes{}, errorf(CodeUnknown, "get compressor: %w", err)
	}

	if _, err := compressor.Write(message); err != nil { // returns uncompressed size, which isn't useful
		_ = m.compressionPool.PutWriter(compressor)
		return sizes{}, errorf(CodeInternal, "couldn't compress data: %w", err)
	}
	if err := m.compressionPool.PutWriter(compressor); err != nil {
		return sizes{}, errorf(CodeInternal, "couldn't close compressor: %w", err)
	}
	wireSize := uint(data.Len())
	if err := m.writeGRPCPrefix(true /* compressed */, trailer, data.Len()); err != nil {
		return sizes{}, err // already enriched
	}
	if _, err := io.Copy(m.writer, data); err != nil {
		if connectErr, ok := asError(err); ok {
			return sizes{}, connectErr
		}
		return sizes{}, errorf(
			CodeUnknown,
			"couldn't write message of length-prefixed message: %w",
			err,
		)
	}
	return sizes{
		Uncompressed: uint(len(message)),
		Wire:         wireSize,
	}, nil
}

func (m *marshaler) writeGRPCPrefix(compressed, trailer bool, size int) *Error {
	prefixes := [5]byte{}
	// The first byte of the prefix is a set of bitwise flags. The least
	// significant bit indicates that the message is compressed, and the most
	// significant bit indicates that it's a block of gRPC-Web trailers.
	if compressed {
		prefixes[0] = flagLPMCompressed
	}
	if trailer {
		prefixes[0] |= flagLPMTrailer
	}
	binary.BigEndian.PutUint32(prefixes[1:5], uint32(size))
	if _, err := m.writer.Write(prefixes[:]); err != nil {
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		return errorf(CodeUnknown, "couldn't write prefix of length-prefixed message: %w", err)
	}
	return nil
}

type unmarshaler struct {
	reader          io.Reader
	max             int64
	codec           Codec
	compressionPool compressionPool

	web        bool
	webTrailer http.Header
}

func (u *unmarshaler) Unmarshal(message any) (sizes, *Error) {
	// Each length-prefixed message starts with 5 bytes of metadata: a one-byte
	// unsigned integer used as a set of bitwise flags, and a four-byte unsigned
	// integer indicating the message length.
	prefixes := make([]byte, 5)
	prefixBytesRead, err := u.reader.Read(prefixes)
	switch {
	case (err == nil || errors.Is(err, io.EOF)) &&
		prefixBytesRead == 5 &&
		(prefixes[0]&flagLPMTrailer != flagLPMTrailer) &&
		isSizeZeroPrefix(prefixes):
		// Successfully read prefix, LPM isn't a trailers block, and expect no
		// additional data, so there's nothing left to do - the zero value of the
		// msg is correct. Zero sizes is correct.
		return sizes{}, nil
	case err != nil && errors.Is(err, io.EOF) && prefixBytesRead == 0:
		// The stream ended cleanly. That's expected, but we need to propagate them
		// to the user so that they know that the stream has ended. We shouldn't
		// add any alarming text about protocol errors, though.
		return sizes{}, NewError(CodeUnknown, err)
	case err != nil || prefixBytesRead < 5:
		// Something else has gone wrong - the stream didn't end cleanly.
		return sizes{}, errorf(
			CodeInvalidArgument,
			"gRPC protocol error: incomplete length-prefixed message prefix: %w", err,
		)
	}

	// The first byte of the prefix is a set of bitwise flags.
	flags := prefixes[0]
	// The least significant bit is the flag for compression.
	compressed := (flags&flagLPMCompressed == flagLPMCompressed)
	// The most significant bit is the flag for gRPC-Web trailers.
	isWebTrailer := u.web && (flags&flagLPMTrailer == flagLPMTrailer)
	// We could check to make sure that the remaining bits are zero, but any
	// non-zero bits are likely flags from a future protocol revision. In a sane
	// world, any new flags would be backward-compatible and safe to ignore.
	// Let's be optimistic!

	wireSize := int(binary.BigEndian.Uint32(prefixes[1:5]))
	if wireSize < 0 {
		return sizes{}, errorf(CodeInvalidArgument, "message size %d overflowed uint32", wireSize)
	} else if u.max > 0 && int64(wireSize) > u.max {
		return sizes{}, errorf(
			CodeInvalidArgument,
			"message size %d is larger than configured max %d",
			wireSize,
			u.max,
		)
	}
	// OPT: easy opportunity to pool buffers and grab the underlying byte slice
	raw := make([]byte, wireSize)
	if wireSize > 0 {
		// At layer 7, we don't know exactly what's happening down in L4. Large
		// length-prefixed messages may arrive in chunks, so we may need to read
		// the request body past EOF. We also need to take care that we don't retry
		// forever if the LPM is malformed.
		remaining := wireSize
		for remaining > 0 {
			bytesRead, err := u.reader.Read(raw[wireSize-remaining : wireSize])
			if err != nil && !errors.Is(err, io.EOF) {
				return sizes{}, errorf(CodeUnknown, "error reading length-prefixed message data: %w", err)
			}
			if errors.Is(err, io.EOF) && prefixBytesRead == 0 {
				// We've gotten zero-length chunk of data. Message is likely malformed,
				// don't wait for additional chunks.
				return sizes{}, errorf(
					CodeInvalidArgument,
					"gRPC protocol error: promised %d bytes in length-prefixed message, got %d bytes",
					wireSize,
					wireSize-remaining,
				)
			}
			remaining -= bytesRead
		}
	}

	if compressed && u.compressionPool == nil {
		return sizes{}, errorf(
			CodeInvalidArgument,
			"gRPC protocol error: sent compressed message without Grpc-Encoding header",
		)
	}

	if wireSize > 0 && compressed {
		decompressor, err := u.compressionPool.GetReader(bytes.NewReader(raw))
		if err != nil {
			return sizes{}, errorf(CodeInvalidArgument, "can't decompress: %w", err)
		}
		// TODO: handle error with user-provided observability hook (#179)
		defer u.compressionPool.PutReader(decompressor) // nolint:errcheck
		// OPT: easy opportunity to pool buffers
		decompressed := bytes.NewBuffer(make([]byte, 0, len(raw)))
		if _, err := decompressed.ReadFrom(decompressor); err != nil {
			return sizes{}, errorf(CodeInvalidArgument, "can't decompress: %w", err)
		}
		raw = decompressed.Bytes()
	}

	if isWebTrailer {
		// Per the gRPC-Web specification, trailers should be encoded as an HTTP/1
		// headers block _without_ the terminating newline. To make the headers
		// parseable by net/textproto, we need to add the newline.
		raw = append(raw, '\n') // nolint:makezero
		bufferedReader := bufio.NewReader(bytes.NewReader(raw))
		mimeReader := textproto.NewReader(bufferedReader)
		mimeHeader, err := mimeReader.ReadMIMEHeader()
		if err != nil {
			return sizes{}, errorf(
				CodeInvalidArgument,
				"gRPC-Web protocol error: received invalid trailers %q: %w",
				string(raw),
				err,
			)
		}
		u.webTrailer = http.Header(mimeHeader)
		return sizes{}, errGotWebTrailers
	}

	if err := u.codec.Unmarshal(raw, message); err != nil {
		return sizes{}, errorf(CodeInvalidArgument, "can't unmarshal into %T: %w", message, err)
	}
	return sizes{
		Wire:         uint(wireSize),
		Uncompressed: uint(len(raw)),
	}, nil
}

func (u *unmarshaler) WebTrailer() http.Header {
	return u.webTrailer
}

func isSizeZeroPrefix(prefix []byte) bool {
	if len(prefix) != 5 {
		return false
	}
	for i := 1; i < 5; i++ {
		if prefix[i] != 0 {
			return false
		}
	}
	return true
}

// sizes reports the wire and uncompressed sizes of a message, which are
// difficult to determine further up the stack.
type sizes struct {
	Wire         uint
	Uncompressed uint
}

func (s sizes) AddTo(stats *Statistics) {
	stats.Messages++
	stats.WireSize += s.Wire
	stats.UncompressedSize += s.Uncompressed
}
