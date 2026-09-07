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

package connect

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect/internal/assert"
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

func TestEnvelopeWriteSendMaxBytes(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name       string
		flags      uint8
		compressed bool
	}{
		{
			name: "rejects_oversized_message_uncompressed",
		},
		{
			name:       "rejects_oversized_message_compressed",
			compressed: true,
		},
		{
			name:  "rejects_oversized_end_stream_uncompressed",
			flags: connectFlagEnvelopeEndStream,
		},
		{
			name:       "rejects_oversized_end_stream_compressed",
			flags:      connectFlagEnvelopeEndStream,
			compressed: true,
		},
		{
			name:  "rejects_oversized_grpc_web_trailer_uncompressed",
			flags: grpcFlagEnvelopeTrailer,
		},
		{
			name:       "rejects_oversized_grpc_web_trailer_compressed",
			flags:      grpcFlagEnvelopeTrailer,
			compressed: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			out := &bytes.Buffer{}
			writer := envelopeWriter{
				sender:       writeSender{writer: out},
				bufferPool:   newBufferPool(),
				sendMaxBytes: 1,
			}
			if testCase.compressed {
				gzipOption, ok := withGzip().(*compressionOption)
				assert.True(t, ok)
				writer.compressMinBytes = 1
				writer.compressionPool = gzipOption.CompressionPool
				writer.bufferPool = newBufferPool()
			}
			env := &envelope{
				Data:  bytes.NewBuffer(make([]byte, 64)),
				Flags: testCase.flags,
			}
			err := writer.Write(env)
			assert.NotNil(t, err)
			assert.Equal(t, CodeOf(err), CodeResourceExhausted)
			assert.True(t, errors.Is(err, errExceedsSendMax))
		})
	}
}

func TestWriteControlFrameFallback(t *testing.T) {
	t.Parallel()
	t.Run("minimal_fallback_fits_under_max", func(t *testing.T) {
		t.Parallel()
		out := &bytes.Buffer{}
		writer := envelopeWriter{
			sender:       writeSender{writer: out},
			bufferPool:   newBufferPool(),
			sendMaxBytes: 128,
		}
		large := strings.Repeat("x", 256)
		end := &connectEndStreamMessage{
			Error: &connectWireError{
				Code:    CodeInternal,
				Message: large,
			},
			Trailer: http.Header{"large-header": []string{large}},
		}
		marshal := func() ([]byte, *Error) {
			data, err := json.Marshal(end)
			if err != nil {
				return nil, errorf(CodeInternal, "marshal: %w", err)
			}
			return data, nil
		}
		reduce := func() {
			end.Trailer = nil
			end.Error = &connectWireError{
				Code:    end.Error.Code,
				Message: "end stream message exceeded sendMaxBytes 128",
			}
		}
		err := writer.writeControlFrame(connectFlagEnvelopeEndStream, marshal, reduce)
		assert.Nil(t, err)
		assert.True(t, out.Len() > 0)
		unmarshaler := connectStreamingUnmarshaler{
			envelopeReader: envelopeReader{
				ctx:        t.Context(),
				reader:     out,
				bufferPool: newBufferPool(),
			},
		}
		readErr := unmarshaler.Unmarshal(nil)
		assert.ErrorIs(t, readErr, errSpecialEnvelope)
		streamErr := unmarshaler.EndStreamError()
		assert.NotNil(t, streamErr)
		assert.Equal(t, streamErr.Code(), CodeInternal)
		assert.Equal(t, streamErr.Message(), "end stream message exceeded sendMaxBytes 128")
		assert.Equal(t, len(unmarshaler.Trailer()), 0)
	})
	t.Run("force_send_when_fallback_exceeds_max", func(t *testing.T) {
		t.Parallel()
		out := &bytes.Buffer{}
		writer := envelopeWriter{
			sender:       writeSender{writer: out},
			bufferPool:   newBufferPool(),
			sendMaxBytes: 1,
		}
		end := &connectEndStreamMessage{
			Error: &connectWireError{
				Code:    CodeResourceExhausted,
				Message: "original",
			},
		}
		marshal := func() ([]byte, *Error) {
			data, err := json.Marshal(end)
			if err != nil {
				return nil, errorf(CodeInternal, "marshal: %w", err)
			}
			return data, nil
		}
		reduce := func() {
			end.Error = &connectWireError{
				Code:    CodeResourceExhausted,
				Message: "end stream message exceeded sendMaxBytes 1",
			}
		}
		err := writer.writeControlFrame(connectFlagEnvelopeEndStream, marshal, reduce)
		assert.Nil(t, err)
		assert.True(t, out.Len() > 0)
	})
}
