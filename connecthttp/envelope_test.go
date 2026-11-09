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
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectgzip"
	"connectrpc.com/connect/v2/internal/assert"
)

//nolint:paralleltest // AllocsPerRun can't run in parallel tests
func TestEnvelopeReaderEndOfStream(t *testing.T) {
	// A clean end of stream is a bare io.EOF with no allocation, and it
	// survives the error translation applied to public APIs.
	source := bytes.NewReader(nil)
	reader := envelopeReader{ctx: t.Context(), reader: source}
	err := reader.Unmarshal(nil)
	assert.True(t, err == io.EOF) //nolint:errorlint // want the exact sentinel
	assert.ErrorIs(t, wrapIfUncoded(err), io.EOF)
	allocs := testing.AllocsPerRun(10, func() {
		_ = reader.Unmarshal(nil)
	})
	// The envelope read buffer is the only allocation.
	assert.True(t, allocs <= 1)
}

func TestCodeStreamEnd(t *testing.T) {
	t.Parallel()
	err := codeStreamEnd(io.EOF)
	assert.ErrorIs(t, err, io.EOF)
	var connectErr *connect.Error
	assert.True(t, errors.As(err, &connectErr))
	assert.Equal(t, connect.CodeUnknown, connectErr.Code())
	assert.False(t, connectErr.IsRemote())
	other := errors.New("other")
	assert.ErrorIs(t, codeStreamEnd(other), other)
	assert.Nil(t, codeStreamEnd(nil))
}

func TestEnvelope(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"number": 42}`)
	const flags = connectFlagEnvelopeEndStream
	frame := envelopeBytes(t, flags, payload)
	t.Run("read", func(t *testing.T) {
		t.Parallel()
		t.Run("full", func(t *testing.T) {
			t.Parallel()
			data := &bytes.Buffer{}
			rdr := envelopeReader{
				reader: bytes.NewReader(frame),
			}
			gotFlags, err := rdr.Read(data)
			assert.Nil(t, err)
			assert.Equal(t, gotFlags, uint8(flags))
			assert.Equal(t, payload, data.Bytes())
		})
		t.Run("byteByByte", func(t *testing.T) {
			t.Parallel()
			data := &bytes.Buffer{}
			rdr := envelopeReader{
				ctx: t.Context(),
				reader: byteByByteReader{
					reader: bytes.NewReader(frame),
				},
			}
			gotFlags, err := rdr.Read(data)
			assert.Nil(t, err)
			assert.Equal(t, gotFlags, uint8(flags))
			assert.Equal(t, payload, data.Bytes())
		})
		t.Run("short", func(t *testing.T) {
			t.Parallel()
			data := &bytes.Buffer{}
			rdr := envelopeReader{
				ctx:    t.Context(),
				reader: bytes.NewReader(frame[:len(frame)-1]),
			}
			_, err := rdr.Read(data)
			assert.Equal(t, err.Error(), fmt.Sprintf(
				"internal: protocol error: promised %d bytes in enveloped message, got %d bytes",
				len(payload), len(payload)-1,
			))
		})
		t.Run("hostileSize", func(t *testing.T) {
			t.Parallel()
			// A prefix claiming 4 GiB with no read limit must not grow the
			// buffer to the claimed size before the bytes arrive.
			prefix := []byte{0, 0xff, 0xff, 0xff, 0xff}
			data := &bytes.Buffer{}
			rdr := envelopeReader{
				ctx:    t.Context(),
				reader: bytes.NewReader(append(prefix, payload...)),
			}
			_, err := rdr.Read(data)
			assert.NotNil(t, err)
			assert.True(t, data.Cap() <= 2*envelopeReadGrowMax)
		})
	})
	t.Run("write", func(t *testing.T) {
		t.Parallel()
		dst := &bytes.Buffer{}
		wtr := envelopeWriter{
			sender: writeSender{writer: dst},
		}
		data := newEnvelopeBuffer()
		_, _ = data.Write(payload)
		assert.Nil(t, wtr.Write(flags, data))
		assert.Equal(t, frame, dst.Bytes())
	})
}

// TestEnvelopeFraming round trips messages through envelopeWriter and
// envelopeReader. Each message must be sent with one write, and sending and
// receiving must not allocate.
func TestEnvelopeFraming(t *testing.T) { //nolint:paralleltest // AllocsPerRun panics in parallel tests
	payload := bytes.Repeat([]byte("framing "), 32)
	for _, compress := range []bool{false, true} { //nolint:paralleltest
		t.Run(fmt.Sprintf("compress=%t", compress), func(t *testing.T) {
			var pool *compressionPool
			if compress {
				pool = newCompressionPool(connectgzip.New())
			}
			var stats connect.MessageStats
			dst := &countingWriter{}
			writer := envelopeWriter{
				ctx:             t.Context(),
				sender:          writeSender{writer: dst},
				codec:           rawCodec{},
				compressionPool: pool,
				stats:           &stats,
			}
			assert.Nil(t, writer.Marshal(&payload))
			assert.Equal(t, dst.writes, 1)
			assert.Equal(t, stats.Size, len(payload))
			assert.Equal(t, stats.CompressedSize > 0, compress)

			reader := envelopeReader{
				ctx:             t.Context(),
				reader:          bytes.NewReader(dst.buf.Bytes()),
				codec:           rawCodec{},
				compressionPool: pool,
			}
			var got []byte
			assert.Nil(t, reader.Unmarshal(&got))
			assert.Equal(t, got, payload)

			// Warm the pools, then count allocations.
			allocs := testing.AllocsPerRun(100, func() {
				dst.reset()
				if err := writer.Marshal(&payload); err != nil {
					t.Fatal(err)
				}
			})
			if !compress {
				assert.Equal(t, allocs, 0)
			}
			frame := bytes.Clone(dst.buf.Bytes())
			src := bytes.NewReader(frame)
			reader.reader = src
			allocs = testing.AllocsPerRun(100, func() {
				src.Reset(frame)
				if err := reader.Unmarshal(&got); err != nil {
					t.Fatal(err)
				}
			})
			if !compress {
				assert.Equal(t, allocs, 0)
			}
		})
	}
}

// TestEnvelopeSendMaxBytes checks that the prefix isn't counted against the
// maximum message size.
func TestEnvelopeSendMaxBytes(t *testing.T) {
	t.Parallel()
	payload := []byte("12345678")
	dst := &countingWriter{}
	writer := envelopeWriter{
		ctx:          t.Context(),
		sender:       writeSender{writer: dst},
		codec:        rawCodec{},
		sendMaxBytes: len(payload),
	}
	assert.Nil(t, writer.Marshal(&payload))
	writer.sendMaxBytes = len(payload) - 1
	err := writer.Marshal(&payload)
	assert.NotNil(t, err)
	assert.Equal(t, err.Code(), connect.CodeResourceExhausted)
}

// rawCodec is a codec for []byte messages.
type rawCodec struct{}

func (rawCodec) Name() string { return "raw" }

func (rawCodec) MarshalWrite(_ context.Context, dst io.Writer, message any) error {
	data, ok := message.(*[]byte)
	if !ok {
		return fmt.Errorf("not *[]byte: %T", message)
	}
	_, err := dst.Write(*data)
	return err
}

func (rawCodec) UnmarshalRead(_ context.Context, src io.Reader, message any) error {
	out, ok := message.(*[]byte)
	buf, isBuf := src.(*bytes.Buffer)
	if !ok || !isBuf {
		return fmt.Errorf("unexpected types %T, %T", message, src)
	}
	*out = append((*out)[:0], buf.Bytes()...)
	return nil
}

// countingWriter records the bytes written and the number of writes.
type countingWriter struct {
	buf    bytes.Buffer
	writes int
}

func (w *countingWriter) Write(data []byte) (int, error) {
	w.writes++
	return w.buf.Write(data)
}

func (w *countingWriter) reset() {
	w.buf.Reset()
	w.writes = 0
}

// TestEnvelopeUnmarshalEndStream tests that envelopeReader.Unmarshal keeps the
// end-stream message when the read draining the rest of the stream fails
// without a code. Over HTTP/1.1, http.Transport closes the connection after a
// failed request body write, so the drain read can return "use of closed
// network connection" even though the client received the entire response.
// See https://github.com/connectrpc/connect-go/issues/1013.
func TestEnvelopeUnmarshalEndStream(t *testing.T) {
	t.Parallel()
	endStreamMessage := []byte(`{"error":{"code":"permission_denied","message":"denied"}}`)
	grpcWebTrailers := []byte("grpc-status: 7\r\ngrpc-message: denied\r\n")
	endStream := envelopeBytes(t, connectFlagEnvelopeEndStream, endStreamMessage)
	trailers := envelopeBytes(t, grpcFlagEnvelopeTrailer, grpcWebTrailers)
	extra := envelopeBytes(t, 0, []byte(`{"number": 42}`))
	testcases := []struct {
		name    string
		input   []byte
		readErr error
		check   func(*testing.T, error, *envelopeReader)
	}{
		{
			name:    "connect_end_stream_uncoded_read_error",
			input:   endStream,
			readErr: net.ErrClosed,
			check: func(t *testing.T, err error, rdr *envelopeReader) {
				t.Helper()
				assert.ErrorIs(t, err, errSpecialEnvelope)
				assert.Equal(t, rdr.lastFlags, uint8(connectFlagEnvelopeEndStream))
				assert.Equal(t, endStreamMessage, rdr.lastData.Bytes())
			},
		},
		{
			name:    "grpc_web_trailers_uncoded_read_error",
			input:   trailers,
			readErr: net.ErrClosed,
			check: func(t *testing.T, err error, rdr *envelopeReader) {
				t.Helper()
				assert.ErrorIs(t, err, errSpecialEnvelope)
				assert.Equal(t, rdr.lastFlags, uint8(grpcFlagEnvelopeTrailer))
				assert.Equal(t, grpcWebTrailers, rdr.lastData.Bytes())
			},
		},
		{
			name:    "extra_bytes_after_end_stream_failed_read",
			input:   append(endStream, extra...),
			readErr: net.ErrClosed,
			check: func(t *testing.T, err error, _ *envelopeReader) {
				t.Helper()
				assert.Equal(t, connect.CodeInternal, connect.CodeOf(err))
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
			check: func(t *testing.T, err error, _ *envelopeReader) {
				t.Helper()
				assert.Equal(t, connect.CodeCanceled, connect.CodeOf(err))
				assert.False(t, errors.Is(err, errSpecialEnvelope))
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
			// Unmarshal doesn't touch the message in the end-stream branch,
			// so a nil message is fine here.
			testcase.check(t, rdr.Unmarshal(nil), &rdr)
		})
	}
}

// envelopeBytes frames a payload with the 5-byte envelope prefix.
func envelopeBytes(t *testing.T, flags uint8, payload []byte) []byte {
	t.Helper()
	frame := []byte{flags}
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(payload)))
	return append(frame, payload...)
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
