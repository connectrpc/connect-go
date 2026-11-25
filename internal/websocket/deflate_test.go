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

package websocket

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"connectrpc.com/connect/v2/internal/assert"
)

const both = "permessage-deflate; server_no_context_takeover; client_no_context_takeover"

func TestNegotiateDeflate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		offers []string
		want   string
	}{
		{name: "none"},
		{name: "bare", offers: []string{"permessage-deflate"}, want: both},
		{name: "chrome", offers: []string{"permessage-deflate; client_max_window_bits"}, want: both},
		{name: "ours", offers: []string{deflateOffer}, want: both},
		{name: "case", offers: []string{"PerMessage-Deflate; Client_No_Context_Takeover"}, want: both},
		{name: "ows", offers: []string{" permessage-deflate\t;\tclient_max_window_bits = 10 ,"}, want: both},
		{name: "quoted", offers: []string{`permessage-deflate; client_max_window_bits="10"`}, want: both},
		{name: "quoted_escape", offers: []string{`permessage-deflate; client_max_window_bits="1\0"`}, want: both},
		{name: "quoted_not_token", offers: []string{`permessage-deflate; client_max_window_bits="1 0"`}},
		{name: "quoted_comma", offers: []string{`permessage-deflate; client_max_window_bits="10", x; y="1"`}, want: both},
		{name: "quoted_comma_not_token", offers: []string{`x; a=",", permessage-deflate`}},
		{name: "duplicate", offers: []string{"permessage-deflate; server_no_context_takeover; server_no_context_takeover"}},
		{name: "unknown_param", offers: []string{"permessage-deflate; foo"}},
		{name: "valued_takeover", offers: []string{"permessage-deflate; client_no_context_takeover=1"}},
		{name: "server_bits_15", offers: []string{"permessage-deflate; server_max_window_bits=15"}, want: both + "; server_max_window_bits=15"},
		{name: "server_bits_quoted_15", offers: []string{`permessage-deflate; server_max_window_bits="15"`}, want: both + "; server_max_window_bits=15"},
		{name: "server_bits_8", offers: []string{"permessage-deflate; server_max_window_bits=8"}},
		{name: "server_bits_14", offers: []string{"permessage-deflate; server_max_window_bits=14"}},
		{name: "server_bits_missing", offers: []string{"permessage-deflate; server_max_window_bits"}},
		{name: "server_bits_015", offers: []string{"permessage-deflate; server_max_window_bits=015"}},
		{name: "server_bits_16", offers: []string{"permessage-deflate; server_max_window_bits=16"}},
		{name: "client_bits_8", offers: []string{"permessage-deflate; client_max_window_bits=8"}, want: both},
		{name: "client_bits_7", offers: []string{"permessage-deflate; client_max_window_bits=7"}},
		{name: "first_declined", offers: []string{"permessage-deflate; server_max_window_bits=10, permessage-deflate; client_max_window_bits"}, want: both},
		{name: "first_declined_lines", offers: []string{"permessage-deflate; server_max_window_bits=9", "permessage-deflate; server_max_window_bits=15"}, want: both + "; server_max_window_bits=15"},
		{name: "other_extension", offers: []string{"x-webkit-deflate-frame, permessage-deflate"}, want: both},
		{name: "only_other", offers: []string{"x-webkit-deflate-frame"}},
		{name: "malformed", offers: []string{"permessage-deflate; =1"}},
		{name: "malformed_later_line", offers: []string{"permessage-deflate", "a b"}},
		{name: "unterminated", offers: []string{`permessage-deflate; client_max_window_bits="10`}},
		{name: "empty_elements", offers: []string{", ,permessage-deflate,,"}, want: both},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := negotiateDeflate(test.offers)
			assert.Equal(t, got, test.want)
			assert.Equal(t, ok, test.want != "")
		})
	}
}

func TestVerifyDeflateResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		values  []string
		want    bool
		wantErr bool
	}{
		{name: "none"},
		{name: "empty_value", values: []string{""}},
		{name: "both", values: []string{both}, want: true},
		{name: "reordered_case", values: []string{"Permessage-Deflate; Client_No_Context_Takeover; Server_No_Context_Takeover"}, want: true},
		{name: "without_client_takeover", values: []string{"permessage-deflate; server_no_context_takeover"}, want: true},
		{name: "server_bits", values: []string{both + "; server_max_window_bits=9"}, want: true},
		{name: "server_bits_quoted", values: []string{both + `; server_max_window_bits="15"`}, want: true},
		{name: "server_bits_invalid", values: []string{both + "; server_max_window_bits=16"}, wantErr: true},
		{name: "server_bits_missing", values: []string{both + "; server_max_window_bits"}, wantErr: true},
		{name: "client_bits", values: []string{both + "; client_max_window_bits=15"}, wantErr: true},
		{name: "without_server_takeover", values: []string{"permessage-deflate; client_no_context_takeover"}, wantErr: true},
		{name: "bare", values: []string{"permessage-deflate"}, wantErr: true},
		{name: "two_elements", values: []string{both + ", " + both}, wantErr: true},
		{name: "two_lines", values: []string{both, both}, wantErr: true},
		{name: "other_name", values: []string{"x-webkit-deflate-frame"}, wantErr: true},
		{name: "duplicate", values: []string{both + "; server_no_context_takeover"}, wantErr: true},
		{name: "unknown_param", values: []string{both + "; foo"}, wantErr: true},
		{name: "valued_server_takeover", values: []string{"permessage-deflate; server_no_context_takeover=1"}, wantErr: true},
		{name: "valued_client_takeover", values: []string{"permessage-deflate; server_no_context_takeover; client_no_context_takeover=1"}, wantErr: true},
		{name: "malformed", values: []string{"permessage-deflate;"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := verifyDeflateResponse(test.values)
			assert.Equal(t, got, test.want)
			assert.Equal(t, err != nil, test.wantErr, assert.Sprintf("error %v", err))
		})
	}
}

func TestCompress(t *testing.T) {
	t.Parallel()
	random := make([]byte, 64<<10)
	for i := range random {
		random[i] = byte(rand.Uint32())
	}
	inputs := map[string][]byte{
		"empty":  {},
		"small":  []byte("hello"),
		"json":   jsonPayload(1 << 10),
		"large":  jsonPayload(300 << 10),
		"random": random,
	}
	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			compressor := getCompressor()
			defer putCompressor(compressor)
			frame, smaller := compressor.compress(input)
			assert.Equal(t, frame[:MaxHeaderSize], make([]byte, MaxHeaderSize))
			payload := frame[MaxHeaderSize:]
			assert.Equal(t, smaller, len(payload) < len(input))
			assert.False(t, bytes.HasSuffix(payload, []byte(syncTail)))
			if len(input) == 0 {
				assert.Equal(t, payload, []byte{0})
			}
			// flate returns io.EOF only after a BFINAL block. Without the
			// final block of deflateTail, a stream that never sets BFINAL
			// inflates fully and then runs out of input.
			output, err := io.ReadAll(flate.NewReader(bytes.NewReader(append(bytes.Clone(payload), syncTail...))))
			assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
			assert.Equal(t, output, input)
		})
	}
	t.Run("threshold", func(t *testing.T) {
		t.Parallel()
		for _, size := range []int{0, 100, 511, 512} {
			var written bytes.Buffer
			conn := newConn(&bufferTransport{buffer: &written}, nil, false)
			conn.deflate = true
			conn.threshold = Options{}.compressionThreshold()
			assert.Nil(t, conn.WriteMessage(MessageText, frameOf(bytes.Repeat([]byte("a"), size))))
			assert.Equal(t, written.Bytes()[0]&rsv1 != 0, size >= 512, assert.Sprintf("size %d", size))
		}
		var written bytes.Buffer
		conn := newConn(&bufferTransport{buffer: &written}, nil, false)
		conn.deflate = true
		conn.threshold = 1
		assert.Nil(t, conn.WriteMessage(MessageBinary, frameOf(random[:1024])))
		assert.Equal(t, written.Bytes()[0], 0x80|byte(opBinary))
	})
}

func TestInflate(t *testing.T) {
	t.Parallel()
	hello := []byte{0xf2, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00}
	bfinalThenBackReference := bfinalStream(t, []byte("Hello, world. Hello, world."))
	// A closed stream ends exactly at its BFINAL block, without the 0x00
	// that RFC 7692 Section 7.2.3.4 shows.
	closed := closedStream(t, []byte(strings.Repeat("Hello, world. ", 8)))
	tests := []struct {
		name     string
		frames   [][]byte
		want     []string
		wantCode StatusCode
	}{
		// RFC 7692 Section 7.2.3.1.
		{name: "one_block", frames: [][]byte{{0xc1, 0x07, 0xf2, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00}}, want: []string{"Hello"}},
		{name: "one_block_fragmented", frames: [][]byte{{0x41, 0x03, 0xf2, 0x48, 0xcd}, {0x80, 0x04, 0xc9, 0xc9, 0x07, 0x00}}, want: []string{"Hello"}},
		// Section 7.2.3.2. Without context takeover the second message's
		// back-reference points before the message.
		{name: "shared_window", frames: [][]byte{rawFrame(0xc1, hello), rawFrame(0xc1, []byte{0xf2, 0x00, 0x11, 0x00, 0x00})}, want: []string{"Hello"}, wantCode: StatusProtocolError},
		{name: "same_bytes_twice", frames: [][]byte{rawFrame(0xc1, hello), rawFrame(0xc1, hello)}, want: []string{"Hello", "Hello"}},
		// Section 7.2.3.3.
		{name: "stored", frames: [][]byte{{0xc1, 0x0b, 0x00, 0x05, 0x00, 0xfa, 0xff, 0x48, 0x65, 0x6c, 0x6c, 0x6f, 0x00}}, want: []string{"Hello"}},
		// Section 7.2.3.4.
		{name: "bfinal", frames: [][]byte{{0xc1, 0x08, 0xf3, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00, 0x00}}, want: []string{"Hello"}},
		// Section 7.2.3.5.
		{name: "two_blocks", frames: [][]byte{{0xc1, 0x0d, 0xf2, 0x48, 0x05, 0x00, 0x00, 0x00, 0xff, 0xff, 0xca, 0xc9, 0xc9, 0x07, 0x00}}, want: []string{"Hello"}},
		// Section 7.2.3.6.
		{name: "empty_fragment", frames: [][]byte{rawFrame(0x41, append(bytes.Clone(hello), syncTail...)), rawFrame(0x80, []byte{0x00})}, want: []string{"Hello"}},
		{name: "empty_message", frames: [][]byte{rawFrame(0xc1, []byte{0x00})}, want: []string{""}},
		{name: "zero_length_payload", frames: [][]byte{rawFrame(0xc2, nil)}, want: []string{""}},
		{name: "zero_length_fragments", frames: [][]byte{rawFrame(0x42, nil), rawFrame(0x00, nil), rawFrame(0x80, nil)}, want: []string{""}},
		{name: "bfinal_back_reference", frames: [][]byte{rawFrame(0xc2, bfinalThenBackReference)}, want: []string{"Hello, world. Hello, world.Hello, world. Hello, world."}},
		{name: "bfinal_back_reference_fragmented", frames: splitFrames(0x02, bfinalThenBackReference, 3), want: []string{"Hello, world. Hello, world.Hello, world. Hello, world."}},
		{name: "bfinal_at_end", frames: [][]byte{rawFrame(0xc2, closed)}, want: []string{strings.Repeat("Hello, world. ", 8)}},
		{name: "bfinal_at_end_empty_fragment", frames: [][]byte{rawFrame(0x42, closed), rawFrame(0x80, nil)}, want: []string{strings.Repeat("Hello, world. ", 8)}},
		{name: "mixed", frames: [][]byte{rawFrame(0xc1, hello), rawFrame(0x81, []byte("plain")), rawFrame(0xc1, hello)}, want: []string{"Hello", "plain", "Hello"}},
		{name: "ping_between_fragments", frames: [][]byte{rawFrame(0x41, hello[:2]), rawFrame(0x89, []byte("p")), rawFrame(0x80, hello[2:])}, want: []string{"Hello"}},
		{name: "truncated", frames: [][]byte{rawFrame(0xc1, hello[:3])}, wantCode: StatusProtocolError},
		{name: "truncated_mid_stored", frames: [][]byte{rawFrame(0xc2, []byte{0x00, 0x00, 0x01, 0xff, 0xfe, 0x48})}, wantCode: StatusProtocolError},
		{name: "final_without_stored_header", frames: [][]byte{rawFrame(0xc1, hello[:6])}, wantCode: StatusProtocolError},
		{name: "garbage", frames: [][]byte{rawFrame(0xc1, []byte{0xff, 0xff, 0xff})}, wantCode: StatusProtocolError},
		{name: "invalid_utf8", frames: [][]byte{rawFrame(0xc1, deflateBytes(t, []byte("a\xffb")))}, wantCode: StatusInvalidPayloadData},
		{name: "utf8_split_rune", frames: [][]byte{rawFrame(0xc1, deflateBytes(t, []byte("h\xc3")))}, wantCode: StatusInvalidPayloadData},
		{name: "rsv1_continuation", frames: [][]byte{rawFrame(0x41, hello[:2]), rawFrame(0xc0, hello[2:])}, wantCode: StatusProtocolError},
		{name: "rsv1_ping", frames: [][]byte{rawFrame(0xc9, nil)}, wantCode: StatusProtocolError},
		{name: "rsv2", frames: [][]byte{rawFrame(0xa1, hello)}, wantCode: StatusProtocolError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			conn, transport := newDeflateTestConn(bytes.Join(test.frames, nil))
			var got []string
			var err error
			for {
				var dst bytes.Buffer
				if _, err = conn.ReadMessage(&dst, 1<<10); err != nil {
					break
				}
				got = append(got, dst.String())
			}
			assert.Equal(t, got, test.want)
			if test.wantCode == 0 {
				assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
				return
			}
			assert.Equal(t, transport.closeCode(), test.wantCode)
			if test.wantCode == StatusProtocolError {
				assert.ErrorIs(t, err, ErrProtocol)
			}
		})
	}
	t.Run("corrupt_wraps_flate_error", func(t *testing.T) {
		t.Parallel()
		conn, _ := newDeflateTestConn(rawFrame(0xc1, []byte{0xff, 0xff, 0xff}))
		_, err := conn.ReadMessage(&bytes.Buffer{}, -1)
		_, ok := errors.AsType[flate.CorruptInputError](err)
		assert.True(t, ok, assert.Sprintf("error %v", err))
	})
	t.Run("discard", func(t *testing.T) {
		t.Parallel()
		conn, _ := newDeflateTestConn(append(rawFrame(0xc1, []byte{0xff}), rawFrame(0xc1, hello)...))
		messageType, err := conn.ReadMessage(nil, -1)
		assert.Nil(t, err)
		assert.Equal(t, messageType, MessageText)
		var dst bytes.Buffer
		_, err = conn.ReadMessage(&dst, -1)
		assert.Nil(t, err)
		assert.Equal(t, dst.String(), "Hello")
	})
}

func TestInflateLimits(t *testing.T) {
	t.Parallel()
	const limit = 64 << 10
	t.Run("zeros", func(t *testing.T) {
		t.Parallel()
		compressed := deflateBytes(t, make([]byte, 1<<20))
		frames := rawFrame(0xc2, compressed)
		counter := &countingReader{reader: bytes.NewReader(frames)}
		conn, transport := newDeflateTestConnFrom(counter)
		var dst bytes.Buffer
		_, err := conn.ReadMessage(&dst, limit)
		assert.ErrorIs(t, err, ErrMessageTooBig)
		assert.Equal(t, transport.closeCode(), StatusMessageTooBig)
		assert.True(t, dst.Len() <= limit)
		// Inflating stopped early. The bufio.Reader reads ahead at most 4 KiB.
		assert.True(t, counter.read <= 2*limit+1<<10+4096+MaxHeaderSize, assert.Sprintf("read %d of %d", counter.read, len(frames)))
	})
	t.Run("output_at_limit", func(t *testing.T) {
		t.Parallel()
		conn, _ := newDeflateTestConn(rawFrame(0xc2, deflateBytes(t, make([]byte, limit))))
		var dst bytes.Buffer
		_, err := conn.ReadMessage(&dst, limit)
		assert.Nil(t, err)
		assert.Equal(t, dst.Len(), limit)
	})
	floods := map[string][]byte{
		"empty_blocks":           rawFrame(0xc2, bytes.Repeat([]byte{0x00, 0x00, 0x00, 0xff, 0xff}, (2*limit+2<<10)/5)),
		"empty_blocks_fragments": bytes.Join(splitFrames(0x02, bytes.Repeat([]byte{0x00, 0x00, 0x00, 0xff, 0xff}, (2*limit+2<<10)/5), 1000), nil),
		// Each final block followed by more payload restarts the stream.
		"final_blocks": rawFrame(0xc2, bytes.Repeat([]byte{0x01, 0x00, 0x00, 0xff, 0xff}, maxRestarts+2)),
	}
	for name, frames := range floods {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			conn, transport := newDeflateTestConn(frames)
			_, err := conn.ReadMessage(&bytes.Buffer{}, limit)
			assert.ErrorIs(t, err, ErrMessageTooBig)
			assert.Equal(t, transport.closeCode(), StatusMessageTooBig)
		})
	}
	t.Run("discard_applies_compressed_bound", func(t *testing.T) {
		t.Parallel()
		conn, transport := newDeflateTestConn(floods["empty_blocks_fragments"])
		_, err := conn.ReadMessage(nil, limit)
		assert.ErrorIs(t, err, ErrMessageTooBig)
		assert.Equal(t, transport.closeCode(), StatusMessageTooBig)
	})
	t.Run("restart_flood", func(t *testing.T) {
		t.Parallel()
		// Each 03 00 is an empty final block, which restarts the stream with
		// 32 KiB of history for 2 bytes of input.
		payload := closedStream(t, bytes.Repeat([]byte("a"), 32<<10))
		payload = append(payload, bytes.Repeat([]byte{0x03, 0x00}, 8000)...)
		payload = append(payload, 0x00)
		for _, readLimit := range []int64{4 << 20, -1} {
			conn, transport := newDeflateTestConn(rawFrame(0xc2, payload))
			_, err := conn.ReadMessage(&bytes.Buffer{}, readLimit)
			assert.ErrorIs(t, err, ErrMessageTooBig, assert.Sprintf("limit %d", readLimit))
			assert.Equal(t, transport.closeCode(), StatusMessageTooBig)
		}
	})
	t.Run("restarts_below_cap", func(t *testing.T) {
		t.Parallel()
		payload := bytes.Repeat([]byte{0x01, 0x00, 0x00, 0xff, 0xff}, maxRestarts+1)
		conn, _ := newDeflateTestConn(rawFrame(0xc2, payload))
		var dst bytes.Buffer
		_, err := conn.ReadMessage(&dst, -1)
		assert.Nil(t, err)
		assert.Equal(t, dst.Len(), 0)
	})
	t.Run("max_int64", func(t *testing.T) {
		t.Parallel()
		conn, _ := newDeflateTestConn(rawFrame(0xc2, deflateBytes(t, bytes.Repeat([]byte("a"), 1000))))
		var dst bytes.Buffer
		_, err := conn.ReadMessage(&dst, math.MaxInt64)
		assert.Nil(t, err)
		assert.Equal(t, dst.Bytes(), bytes.Repeat([]byte("a"), 1000))
	})
	t.Run("unlimited", func(t *testing.T) {
		t.Parallel()
		conn, _ := newDeflateTestConn(rawFrame(0xc2, deflateBytes(t, make([]byte, 1<<20))))
		var dst bytes.Buffer
		_, err := conn.ReadMessage(&dst, -1)
		assert.Nil(t, err)
		assert.Equal(t, dst.Len(), 1<<20)
	})
	assert.Equal(t, compressedLimit(-1), -1)
	assert.Equal(t, compressedLimit(0), 1<<10)
	assert.Equal(t, compressedLimit(1<<62), -1)
}

// FuzzInflate compares inflating a compressed message with compress/flate.
// The message must match flate's output, or fail the connection with 1002,
// 1007 or 1009, and never yield more than the limit.
func FuzzInflate(f *testing.F) {
	const limit = 256
	f.Add([]byte{0xf2, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00}, uint8(0), false)
	f.Add([]byte{0xf3, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00, 0x00}, uint8(3), true)
	f.Add([]byte{0x00, 0x05, 0x00, 0xfa, 0xff, 0x48, 0x65, 0x6c, 0x6c, 0x6f, 0x00}, uint8(5), false)
	f.Add([]byte{0x00}, uint8(0), true)
	f.Add([]byte{}, uint8(0), false)
	f.Add([]byte{0x01, 0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff}, uint8(2), false)
	f.Fuzz(func(t *testing.T, payload []byte, fragment uint8, text bool) {
		first := byte(opBinary)
		if text {
			first = byte(opText)
		}
		frames := bytes.Join(splitFrames(first, payload, int(fragment)+1), nil)
		conn, transport := newDeflateTestConn(frames)
		var dst bytes.Buffer
		_, err := conn.ReadMessage(&dst, limit)
		want, restarts, wantErr := referenceInflate(payload)
		if err == nil {
			assert.Nil(t, wantErr)
			assert.Equal(t, dst.Bytes(), want)
			assert.True(t, dst.Len() <= limit)
			return
		}
		assert.True(t, dst.Len() <= limit)
		code := transport.closeCode()
		assert.True(t, code == StatusProtocolError || code == StatusInvalidPayloadData || code == StatusMessageTooBig, assert.Sprintf("code %d error %v", code, err))
		if wantErr == nil && len(want) <= limit && restarts == 0 && (!text || utf8.Valid(want)) {
			t.Fatalf("reference inflated %d bytes, got %v", len(want), err)
		}
	})
}

// countingReader counts the bytes read through it.
type countingReader struct {
	reader io.Reader
	read   int
}

func (c *countingReader) Read(data []byte) (int, error) {
	size, err := c.reader.Read(data)
	c.read += size
	return size, err
}

// recordingTransport reads from a reader and records writes.
type recordingTransport struct {
	io.Reader

	written bytes.Buffer
}

func (r *recordingTransport) Write(data []byte) (int, error) {
	return r.written.Write(data)
}

func (*recordingTransport) Close() error {
	return nil
}

// closeCode returns the code of the last Close frame the Conn wrote.
func (r *recordingTransport) closeCode() StatusCode {
	reader := bufio.NewReader(bytes.NewReader(r.written.Bytes()))
	var code StatusCode
	for {
		hdr, err := readHeader(reader)
		if err != nil {
			return code
		}
		payload := make([]byte, hdr.length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return code
		}
		if hdr.masked {
			mask(payload, hdr.maskKey, 0)
		}
		if hdr.opcode == opClose && len(payload) >= 2 {
			code = StatusCode(binary.BigEndian.Uint16(payload))
		}
	}
}

// newDeflateTestConn is a client Conn with compression, reading frames from
// a server.
func newDeflateTestConn(frames []byte) (*Conn, *recordingTransport) {
	return newDeflateTestConnFrom(bytes.NewReader(frames))
}

func newDeflateTestConnFrom(reader io.Reader) (*Conn, *recordingTransport) {
	transport := &recordingTransport{Reader: reader}
	conn := newDeflateConn(transport, bufio.NewReader(transport), true, true, Options{})
	return conn, transport
}

// rawFrame builds an unmasked frame with the given first byte.
func rawFrame(first byte, payload []byte) []byte {
	var space [MaxHeaderSize]byte
	start := putHeader(space[:], opcode(first&0x0f), first&0x70, len(payload), false)
	frame := append(space[start:], payload...)
	if first&0x80 == 0 {
		frame[0] &^= 0x80
	}
	return frame
}

// splitFrames splits a compressed message into unmasked frames of up to size
// bytes.
func splitFrames(dataOpcode byte, payload []byte, size int) [][]byte {
	var frames [][]byte
	first := dataOpcode | rsv1
	for {
		chunk := payload[:min(size, len(payload))]
		payload = payload[len(chunk):]
		if len(payload) == 0 {
			return append(frames, rawFrame(0x80|first, chunk))
		}
		frames = append(frames, rawFrame(first, chunk))
		first = byte(opContinuation)
	}
}

// deflateBytes compresses data as a sender following RFC 7692 does.
func deflateBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := flate.NewWriter(&output, flate.BestSpeed)
	assert.Nil(t, err)
	_, err = writer.Write(data)
	assert.Nil(t, err)
	assert.Nil(t, writer.Flush())
	return bytes.TrimSuffix(output.Bytes(), []byte(syncTail))
}

// bfinalStream compresses text, closing the stream with BFINAL, then
// compresses it again with the first copy as dictionary. The second part
// refers back across the BFINAL block.
func bfinalStream(t *testing.T, text []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := flate.NewWriter(&output, flate.BestSpeed)
	assert.Nil(t, err)
	_, err = writer.Write(text)
	assert.Nil(t, err)
	assert.Nil(t, writer.Close())
	first := output.Len()
	writer, err = flate.NewWriterDict(&output, flate.DefaultCompression, text)
	assert.Nil(t, err)
	_, err = writer.Write(text)
	assert.Nil(t, err)
	assert.Nil(t, writer.Flush())
	// Without the history, the second part doesn't inflate.
	_, err = io.ReadAll(flate.NewReader(io.MultiReader(bytes.NewReader(output.Bytes()[first:]), strings.NewReader(deflateTail))))
	assert.NotNil(t, err)
	return bytes.TrimSuffix(output.Bytes(), []byte(syncTail))
}

// closedStream compresses text and closes the stream, which ends it with a
// BFINAL block.
func closedStream(t *testing.T, text []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := flate.NewWriter(&output, flate.BestSpeed)
	assert.Nil(t, err)
	_, err = writer.Write(text)
	assert.Nil(t, err)
	assert.Nil(t, writer.Close())
	return output.Bytes()
}

// referenceInflate inflates payload with compress/flate, restarting with
// the output as history after each BFINAL block until the input runs out. An
// empty payload is an empty message.
func referenceInflate(payload []byte) ([]byte, int, error) {
	if len(payload) == 0 {
		return []byte{}, 0, nil
	}
	source := bytes.NewReader(append(bytes.Clone(payload), deflateTail...))
	var output bytes.Buffer
	restarts := 0
	for {
		dict := output.Bytes()[max(0, output.Len()-1<<maxWindowBits):]
		if _, err := output.ReadFrom(flate.NewReaderDict(source, dict)); err != nil {
			return nil, restarts, err
		}
		// What's left after the payload is deflateTail, which can't start
		// a stream.
		if source.Len() <= len(deflateTail) {
			return output.Bytes(), restarts, nil
		}
		restarts++
		if output.Len() > 1<<20 || restarts > 1<<16 {
			return nil, restarts, fmt.Errorf("reference gave up after %d restarts", restarts)
		}
	}
}

// jsonPayload is about size bytes of JSON-like text.
func jsonPayload(size int) []byte {
	var payload bytes.Buffer
	payload.WriteString(`{"items":[`)
	for i := 0; payload.Len() < size-2; i++ {
		if i > 0 {
			payload.WriteByte(',')
		}
		fmt.Fprintf(&payload, `{"id":%d,"name":"item-%d","price":%d.%02d,"tags":["alpha","beta"],"active":%t}`, i, i*7919%10007, i*31%997, i%100, i%3 == 0)
	}
	payload.WriteString("]}")
	return payload.Bytes()[:size]
}
