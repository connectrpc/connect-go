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
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
)

func TestEnvelope(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"number": 42}`)
	head, err := makeEnvelopePrefix(0, len(payload))
	assert.Nil(t, err)
	buf := &bytes.Buffer{}
	buf.Write(head[:])
	buf.Write(payload)
	t.Run("read", func(t *testing.T) {
		t.Parallel()
		t.Run("full", func(t *testing.T) {
			t.Parallel()
			env := &envelope{Data: &bytes.Buffer{}}
			rdr := envelopeReader{
				reader: bytes.NewReader(buf.Bytes()),
			}
			assert.Nil(t, rdr.Read(env))
			assert.Equal(t, payload, env.Data.Bytes())
		})
		t.Run("byteByByte", func(t *testing.T) {
			t.Parallel()
			env := &envelope{Data: &bytes.Buffer{}}
			rdr := envelopeReader{
				ctx: t.Context(),
				reader: byteByByteReader{
					reader: bytes.NewReader(buf.Bytes()),
				},
			}
			assert.Nil(t, rdr.Read(env))
			assert.Equal(t, payload, env.Data.Bytes())
		})
	})
	t.Run("write", func(t *testing.T) {
		t.Parallel()
		t.Run("full", func(t *testing.T) {
			t.Parallel()
			dst := &bytes.Buffer{}
			wtr := envelopeWriter{
				sender: writeSender{writer: dst},
			}
			env := &envelope{Data: bytes.NewBuffer(payload)}
			err := wtr.Write(env)
			assert.Nil(t, err)
			assert.Equal(t, buf.Bytes(), dst.Bytes())
		})
		t.Run("partial", func(t *testing.T) {
			t.Parallel()
			dst := &bytes.Buffer{}
			env := &envelope{Data: bytes.NewBuffer(payload)}
			_, err := io.CopyN(dst, env, 2)
			assert.Nil(t, err)
			_, err = env.WriteTo(dst)
			assert.Nil(t, err)
			assert.Equal(t, buf.Bytes(), dst.Bytes())
		})
	})
	t.Run("seek", func(t *testing.T) {
		t.Parallel()
		t.Run("start", func(t *testing.T) {
			t.Parallel()
			dst1 := &bytes.Buffer{}
			dst2 := &bytes.Buffer{}
			env := &envelope{Data: bytes.NewBuffer(payload)}
			_, err := io.CopyN(dst1, env, 2)
			assert.Nil(t, err)
			assert.Equal(t, env.Len(), len(payload)+3)
			_, err = env.Seek(0, io.SeekStart)
			assert.Nil(t, err)
			assert.Equal(t, env.Len(), len(payload)+5)
			_, err = io.CopyN(dst2, env, 2)
			assert.Nil(t, err)
			assert.Equal(t, dst1.Bytes(), dst2.Bytes())
			_, err = env.WriteTo(dst2)
			assert.Nil(t, err)
			assert.Equal(t, dst2.Bytes(), buf.Bytes())
			assert.Equal(t, env.Len(), 0)
		})
	})
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
		check   func(*testing.T, *connect.Error, *envelopeReader)
	}{
		{
			name:    "connect_end_stream_uncoded_read_error",
			input:   endStream,
			readErr: net.ErrClosed,
			check: func(t *testing.T, err *connect.Error, rdr *envelopeReader) {
				assert.ErrorIs(t, err, errSpecialEnvelope)
				assert.Equal(t, rdr.last.Flags, uint8(connectFlagEnvelopeEndStream))
				assert.Equal(t, endStreamMessage, rdr.last.Data.Bytes())
			},
		},
		{
			name:    "grpc_web_trailers_uncoded_read_error",
			input:   trailers,
			readErr: net.ErrClosed,
			check: func(t *testing.T, err *connect.Error, rdr *envelopeReader) {
				assert.ErrorIs(t, err, errSpecialEnvelope)
				assert.Equal(t, rdr.last.Flags, uint8(grpcFlagEnvelopeTrailer))
				assert.Equal(t, grpcWebTrailers, rdr.last.Data.Bytes())
			},
		},
		{
			name:    "extra_bytes_after_end_stream_failed_read",
			input:   append(endStream, extra...),
			readErr: net.ErrClosed,
			check: func(t *testing.T, err *connect.Error, _ *envelopeReader) {
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
			check: func(t *testing.T, err *connect.Error, _ *envelopeReader) {
				assert.Equal(t, connect.CodeCanceled, err.Code())
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
	head, err := makeEnvelopePrefix(flags, len(payload))
	assert.Nil(t, err)
	return append(head[:], payload...)
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
