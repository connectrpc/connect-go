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
	"net"
	"net/http"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectgzip"
	"connectrpc.com/connect/v2/internal/assert"
	"connectrpc.com/connect/v2/internal/bufferpool"
)

func TestEnvelope(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"number": 42}`)
	framed := envelopeBytes(0, payload)
	t.Run("read", func(t *testing.T) {
		t.Parallel()
		t.Run("full", func(t *testing.T) {
			t.Parallel()
			rdr := envelopeReader{
				ctx:    t.Context(),
				reader: bytes.NewReader(framed),
			}
			data, flags, err := rdr.Read()
			defer bufferpool.Put(data)
			assert.Nil(t, err)
			assert.Equal(t, flags, uint8(0))
			assert.Equal(t, payload, data.Bytes())
			assert.Equal(t, rdr.bytesRead, int64(len(framed)))
		})
		t.Run("byteByByte", func(t *testing.T) {
			t.Parallel()
			rdr := envelopeReader{
				ctx: t.Context(),
				reader: byteByByteReader{
					reader: bytes.NewReader(framed),
				},
			}
			data, _, err := rdr.Read()
			defer bufferpool.Put(data)
			assert.Nil(t, err)
			assert.Equal(t, payload, data.Bytes())
		})
		t.Run("incompletePrefix", func(t *testing.T) {
			t.Parallel()
			rdr := envelopeReader{
				ctx:    t.Context(),
				reader: bytes.NewReader(framed[:3]),
			}
			data, _, err := rdr.Read()
			defer bufferpool.Put(data)
			assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
		})
		t.Run("incompleteMessage", func(t *testing.T) {
			t.Parallel()
			rdr := envelopeReader{
				ctx:    t.Context(),
				reader: bytes.NewReader(framed[:len(framed)-1]),
			}
			data, _, err := rdr.Read()
			defer bufferpool.Put(data)
			assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
			assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
		})
		t.Run("cleanEOF", func(t *testing.T) {
			t.Parallel()
			rdr := envelopeReader{
				ctx:    t.Context(),
				reader: bytes.NewReader(nil),
			}
			data, _, err := rdr.Read()
			defer bufferpool.Put(data)
			assert.ErrorIs(t, err, io.EOF)
		})
	})
	t.Run("write", func(t *testing.T) {
		t.Parallel()
		dst := &bytes.Buffer{}
		wtr := envelopeWriter{
			sender: writeSender{writer: dst},
		}
		buffer := &bytes.Buffer{}
		padEnvelope(buffer)
		buffer.Write(payload)
		assert.Nil(t, wtr.Write(0, buffer))
		assert.Equal(t, framed, dst.Bytes())
	})
}

// TestEnvelopeCompressed tests that a message is compressed on write and
// decompressed on read, including a message ending the stream.
func TestEnvelopeCompressed(t *testing.T) {
	t.Parallel()
	pool := newCompressionPool(connectgzip.New())
	payload := bytes.Repeat([]byte("hello "), 100)
	stream := &bytes.Buffer{}
	wtr := envelopeWriter{
		sender:          writeSender{writer: stream},
		compressionPool: pool,
	}
	for _, flags := range []uint8{0, connectFlagEnvelopeEndStream} {
		buffer := &bytes.Buffer{}
		padEnvelope(buffer)
		buffer.Write(payload)
		assert.Nil(t, wtr.Write(flags, buffer))
	}
	assert.True(t, stream.Len() < 2*len(payload))

	rdr := envelopeReader{
		ctx:             t.Context(),
		reader:          stream,
		compressionPool: pool,
	}
	for _, wantFlags := range []uint8{0, connectFlagEnvelopeEndStream} {
		data, flags, err := rdr.Read()
		assert.Nil(t, err)
		assert.Equal(t, flags, wantFlags|flagEnvelopeCompressed)
		assert.Equal(t, payload, data.Bytes())
		bufferpool.Put(data)
	}
}

// TestEnvelopeReadGrowsAsBytesArrive tests that a peer claiming a large
// message doesn't make Read allocate for it before the bytes arrive.
func TestEnvelopeReadGrowsAsBytesArrive(t *testing.T) {
	t.Parallel()
	var prefix [envelopePrefixSize]byte
	binary.BigEndian.PutUint32(prefix[1:], 1<<30)
	rdr := envelopeReader{
		ctx:    t.Context(),
		reader: io.MultiReader(bytes.NewReader(prefix[:]), bytes.NewReader(make([]byte, 10))),
	}
	data, _, err := rdr.Read()
	assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.True(t, data.Cap() < 64*1024)
}

// TestEnvelopeReadEndStream tests that Read keeps the end-stream message
// when the read draining the rest of the stream fails without a code. Over
// HTTP/1.1, http.Transport closes the connection after a failed request body
// write, so the drain read can return "use of closed network connection" even
// though the client received the entire response.
// See https://github.com/connectrpc/connect-go/issues/1013.
func TestEnvelopeReadEndStream(t *testing.T) {
	t.Parallel()
	endStreamMessage := []byte(`{"error":{"code":"permission_denied","message":"denied"}}`)
	grpcWebTrailers := []byte("grpc-status: 7\r\ngrpc-message: denied\r\n")
	endStream := envelopeBytes(connectFlagEnvelopeEndStream, endStreamMessage)
	trailers := envelopeBytes(grpcFlagEnvelopeTrailer, grpcWebTrailers)
	extra := envelopeBytes(0, []byte(`{"number": 42}`))
	testcases := []struct {
		name      string
		input     []byte
		readErr   error
		wantFlags uint8
		wantData  []byte
		check     func(*testing.T, *connect.Error)
	}{
		{
			name:      "connect_end_stream_uncoded_read_error",
			input:     endStream,
			readErr:   net.ErrClosed,
			wantFlags: connectFlagEnvelopeEndStream,
			wantData:  endStreamMessage,
		},
		{
			name:      "grpc_web_trailers_uncoded_read_error",
			input:     trailers,
			readErr:   net.ErrClosed,
			wantFlags: grpcFlagEnvelopeTrailer,
			wantData:  grpcWebTrailers,
		},
		{
			name:    "extra_bytes_after_end_stream_failed_read",
			input:   append(endStream, extra...),
			readErr: net.ErrClosed,
			check: func(t *testing.T, err *connect.Error) {
				t.Helper()
				assert.Equal(t, connect.CodeInternal, err.Code())
				assert.Equal(t,
					fmt.Sprintf("internal: corrupt response: %d extra bytes after end of stream", len(extra)),
					err.Error(),
				)
			},
		},
		{
			name:    "coded_read_error_takes_precedence",
			input:   endStream,
			readErr: fmt.Errorf("read: %w", context.Canceled),
			check: func(t *testing.T, err *connect.Error) {
				t.Helper()
				assert.Equal(t, connect.CodeCanceled, err.Code())
			},
		},
	}
	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			t.Parallel()
			rdr := envelopeReader{
				ctx: t.Context(),
				reader: errorAfterBodyReader{
					reader: bytes.NewReader(testcase.input),
					err:    testcase.readErr,
				},
			}
			data, flags, err := rdr.Read()
			defer bufferpool.Put(data)
			if testcase.check != nil {
				testcase.check(t, err)
				return
			}
			assert.Nil(t, err)
			assert.Equal(t, flags, testcase.wantFlags)
			assert.Equal(t, testcase.wantData, data.Bytes())
		})
	}
}

// TestEnvelopeEndStreamCompressed tests that a compressed end of stream is
// decompressed before it's parsed.
func TestEnvelopeEndStreamCompressed(t *testing.T) {
	t.Parallel()
	pool := newCompressionPool(connectgzip.New())
	stream := &bytes.Buffer{}
	marshaler := connectStreamingMarshaler{
		envelopeWriter: envelopeWriter{
			sender:          writeSender{writer: stream},
			compressionPool: pool,
		},
	}
	trailer := http.Header{"Key": {"value"}}
	assert.Nil(t, marshaler.MarshalEndStream(connect.NewError(connect.CodeNotFound, "missing"), trailer))
	assert.Equal(t, stream.Bytes()[0], uint8(connectFlagEnvelopeEndStream|flagEnvelopeCompressed))

	unmarshaler := connectStreamingUnmarshaler{
		envelopeReader: envelopeReader{
			ctx:             t.Context(),
			reader:          stream,
			compressionPool: pool,
		},
	}
	err := unmarshaler.Unmarshal(nil) // the end of stream doesn't use the message
	assert.ErrorIs(t, err, errSpecialEnvelope)
	assert.Equal(t, unmarshaler.Trailer().Get("Key"), "value")
	assert.Equal(t, connect.CodeOf(unmarshaler.EndStreamError()), connect.CodeNotFound)
}

// envelopeBytes frames a payload with the 5-byte envelope prefix.
func envelopeBytes(flags uint8, payload []byte) []byte {
	framed := make([]byte, envelopePrefixSize, envelopePrefixSize+len(payload))
	framed[0] = flags
	binary.BigEndian.PutUint32(framed[1:], uint32(len(payload)))
	return append(framed, payload...)
}

// errorAfterBodyReader is a test reader that returns the configured error
// where the wrapped reader would return io.EOF, as if the connection were
// closed right after the last byte of the body.
type errorAfterBodyReader struct {
	reader io.Reader
	err    error
}

func (r errorAfterBodyReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	if errors.Is(err, io.EOF) {
		return n, r.err
	}
	return n, err
}

// byteByByteReader is test reader that reads a single byte at a time.
type byteByByteReader struct {
	reader io.ByteReader
}

func (b byteByByteReader) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	next, err := b.reader.ReadByte()
	if err != nil {
		return 0, err
	}
	data[0] = next
	return 1, nil
}
