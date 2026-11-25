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
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"connectrpc.com/connect/v2/internal/assert"
)

func TestPutHeaderRoundTrip(t *testing.T) {
	t.Parallel()
	for _, length := range []int{0, 1, 125, 126, 65535, 65536, 1 << 20} {
		for _, client := range []bool{false, true} {
			var space [MaxHeaderSize]byte
			start := putHeader(space[:], opBinary, 0, length, client)
			wantSize := 2
			switch {
			case length > 65535:
				wantSize += 8
			case length > 125:
				wantSize += 2
			}
			if client {
				wantSize += 4
			}
			assert.Equal(t, MaxHeaderSize-start, wantSize, assert.Sprintf("length %d client %t", length, client))
			hdr, err := readHeader(bufio.NewReader(bytes.NewReader(space[start:])))
			assert.Nil(t, err)
			assert.True(t, hdr.fin)
			assert.Equal(t, hdr.rsv, 0)
			assert.Equal(t, hdr.opcode, opBinary)
			assert.Equal(t, hdr.length, int64(length))
			assert.Equal(t, hdr.masked, client)
			if client {
				assert.Equal(t, hdr.maskKey, [4]byte(space[MaxHeaderSize-4:]))
			}
		}
	}
}

func TestReadHeader(t *testing.T) {
	t.Parallel()
	t.Run("length_msb", func(t *testing.T) {
		t.Parallel()
		hdr, err := readHeader(bufio.NewReader(bytes.NewReader([]byte{0x82, 0x7f, 0x80, 0, 0, 0, 0, 0, 0, 0})))
		assert.Nil(t, err)
		assert.True(t, hdr.length < 0)
		assert.NotNil(t, hdr.check(true, false))
	})
	t.Run("non_minimal_length", func(t *testing.T) {
		t.Parallel()
		hdr, err := readHeader(bufio.NewReader(bytes.NewReader([]byte{0x82, 0x7e, 0, 5})))
		assert.Nil(t, err)
		assert.Equal(t, hdr.length, 5)
	})
	t.Run("truncated", func(t *testing.T) {
		t.Parallel()
		for _, data := range [][]byte{{}, {0x82}, {0x82, 0x7e, 0}, {0x82, 0x80, 1, 2}} {
			_, err := readHeader(bufio.NewReader(bytes.NewReader(data)))
			assert.NotNil(t, err)
		}
	})
}

func TestHeaderCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		hdr     header
		client  bool
		deflate bool
		valid   bool
	}{
		{name: "text", hdr: header{fin: true, opcode: opText, masked: true}, valid: true},
		{name: "client_unmasked", hdr: header{fin: true, opcode: opText}, client: true, valid: true},
		{name: "unmasked_from_client", hdr: header{fin: true, opcode: opText}},
		{name: "masked_from_server", hdr: header{fin: true, opcode: opText, masked: true}, client: true},
		{name: "rsv1", hdr: header{fin: true, rsv: 0x40, opcode: opText, masked: true}},
		{name: "rsv1_binary", hdr: header{fin: true, rsv: 0x40, opcode: opBinary, masked: true}},
		{name: "rsv1_text_deflate", hdr: header{fin: true, rsv: 0x40, opcode: opText, masked: true}, deflate: true, valid: true},
		{name: "rsv1_binary_deflate", hdr: header{rsv: 0x40, opcode: opBinary, masked: true}, deflate: true, valid: true},
		{name: "rsv1_client_deflate", hdr: header{fin: true, rsv: 0x40, opcode: opBinary}, client: true, deflate: true, valid: true},
		{name: "rsv1_continuation_deflate", hdr: header{fin: true, rsv: 0x40, opcode: opContinuation, masked: true}, deflate: true},
		{name: "rsv1_ping_deflate", hdr: header{fin: true, rsv: 0x40, opcode: opPing, masked: true}, deflate: true},
		{name: "rsv1_pong_deflate", hdr: header{fin: true, rsv: 0x40, opcode: opPong, masked: true}, deflate: true},
		{name: "rsv1_close_deflate", hdr: header{fin: true, rsv: 0x40, opcode: opClose, masked: true}, deflate: true},
		{name: "rsv2_deflate", hdr: header{fin: true, rsv: 0x20, opcode: opText, masked: true}, deflate: true},
		{name: "rsv3", hdr: header{fin: true, rsv: 0x10, opcode: opText, masked: true}},
		{name: "rsv3_deflate", hdr: header{fin: true, rsv: 0x10, opcode: opText, masked: true}, deflate: true},
		{name: "rsv12_deflate", hdr: header{fin: true, rsv: 0x60, opcode: opText, masked: true}, deflate: true},
		{name: "opcode_3", hdr: header{fin: true, opcode: 3, masked: true}},
		{name: "opcode_11", hdr: header{fin: true, opcode: 11, masked: true}},
		{name: "fragmented_ping", hdr: header{opcode: opPing, masked: true}},
		{name: "ping_125", hdr: header{fin: true, opcode: opPing, masked: true, length: 125}, valid: true},
		{name: "ping_126", hdr: header{fin: true, opcode: opPing, masked: true, length: 126}},
		{name: "negative_length", hdr: header{fin: true, opcode: opBinary, masked: true, length: -1}},
		{name: "fragmented_text", hdr: header{opcode: opText, masked: true, length: 1 << 40}, valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.hdr.check(test.client, test.deflate)
			assert.Equal(t, err == nil, test.valid, assert.Sprintf("error %v", err))
		})
	}
}

func TestMask(t *testing.T) {
	t.Parallel()
	key := [4]byte{0xa1, 0xb2, 0xc3, 0xd4}
	for length := range 40 {
		for start := range 4 {
			data := make([]byte, length)
			for i := range data {
				data[i] = byte(i * 7)
			}
			want := bytes.Clone(data)
			for i := range want {
				want[i] ^= key[(start+i)%4]
			}
			// Masking in two pieces must match masking at once.
			split := length / 3
			pos := mask(data[:split], key, start)
			pos = mask(data[split:], key, pos)
			assert.Equal(t, data, want, assert.Sprintf("length %d start %d", length, start))
			assert.Equal(t, pos, (start+length)%4)
		}
	}
}

func TestUTF8Validator(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"", "hello", "héllo", "日本語", "😀x😀", "\xef\xbf\xbd",
		"\xff", "a\xc3", "\xc3(", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xe0\x80\xaf", "😀\x80",
	}
	for _, input := range inputs {
		want := utf8.ValidString(input)
		for split := range len(input) + 1 {
			var validator utf8Validator
			got := validator.valid([]byte(input[:split])) && validator.valid([]byte(input[split:])) && validator.complete()
			assert.Equal(t, got, want, assert.Sprintf("%q split at %d", input, split))
		}
		var validator utf8Validator
		got := true
		for i := range len(input) {
			got = got && validator.valid([]byte{input[i]})
		}
		assert.Equal(t, got && validator.complete(), want, assert.Sprintf("%q byte by byte", input))
	}
}

func TestParseClose(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		payload  []byte
		want     *CloseError
		wantCode StatusCode
	}{
		{name: "empty", want: &CloseError{Code: StatusNoStatusReceived}},
		{name: "one_byte", payload: []byte{3}, wantCode: StatusProtocolError},
		{name: "normal", payload: []byte{0x03, 0xe8, 'o', 'k'}, want: &CloseError{Code: StatusNormalClosure, Reason: "ok"}},
		{name: "1005", payload: []byte{0x03, 0xed}, wantCode: StatusProtocolError},
		{name: "reason_utf8", payload: []byte{0x03, 0xe8, 0xff}, wantCode: StatusInvalidPayloadData},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			closeErr, code, err := parseClose(test.payload)
			assert.Equal(t, closeErr, test.want)
			assert.Equal(t, code, test.wantCode)
			assert.Equal(t, err != nil, test.wantCode != 0)
		})
	}
}

func TestCloseReason(t *testing.T) {
	t.Parallel()
	assert.Equal(t, closeReason("bye"), "bye")
	assert.Equal(t, closeReason("a\xffb"), "ab")
	long := strings.Repeat("x", maxCloseReason-1) + "é"
	assert.Equal(t, closeReason(long), strings.Repeat("x", maxCloseReason-1))
	assert.Equal(t, len(closeReason(strings.Repeat("x", 200))), maxCloseReason)
}

func TestValidCloseCode(t *testing.T) {
	t.Parallel()
	for _, code := range []StatusCode{1000, 1001, 1002, 1003, 1007, 1011, 1014, 3000, 4999} {
		assert.True(t, validCloseCode(code), assert.Sprintf("code %d", code))
	}
	for _, code := range []StatusCode{0, 999, 1004, 1005, 1006, 1015, 2999, 5000} {
		assert.False(t, validCloseCode(code), assert.Sprintf("code %d", code))
	}
}

// FuzzReadMessage reads arbitrary bytes as frames from a peer, with and
// without compression. Reads must end in an error without panicking, every
// message must respect the read limit and UTF-8 rules, and the call after an
// error must return the same error.
func FuzzReadMessage(f *testing.F) {
	const readLimit = 64
	f.Add(false, false, maskedFrame(opText, "hi"))
	f.Add(true, false, []byte{0x82, 0x03, 'a', 'b', 'c'})
	f.Add(true, false, []byte{0x01, 0x01, 'a', 0x89, 0x00, 0x80, 0x01, 'b', 0x88, 0x02, 0x03, 0xe8})
	f.Add(true, false, []byte{0x82, 0x7f, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff})
	f.Add(true, false, []byte{0x81, 0x02, 0xc3, 0x28})
	f.Add(false, false, []byte{0x88, 0x82, 0, 0, 0, 0, 0x03, 0xe8})
	f.Add(true, true, []byte{0xc1, 0x07, 0xf2, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00})
	f.Add(true, true, []byte{0x41, 0x03, 0xf2, 0x48, 0xcd, 0x89, 0x00, 0x80, 0x04, 0xc9, 0xc9, 0x07, 0x00})
	f.Add(true, true, []byte{0xc1, 0x08, 0xf3, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00, 0x00, 0x82, 0x01, 'x'})
	f.Add(true, true, []byte{0xc2, 0x00, 0xc1, 0x01, 0x00})
	f.Add(true, true, []byte{0x41, 0x01, 0xf2, 0xc0, 0x01, 0x00})
	f.Add(true, false, []byte{0xc1, 0x07, 0xf2, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00})
	f.Add(false, true, []byte{0xc2, 0x85, 0, 0, 0, 0, 0x00, 0x00, 0x00, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, client, deflate bool, data []byte) {
		transport := &fuzzTransport{Reader: bytes.NewReader(data)}
		conn := newDeflateConn(transport, bufio.NewReader(transport), client, deflate, Options{})
		var dst bytes.Buffer
		for {
			dst.Reset()
			messageType, err := conn.ReadMessage(&dst, readLimit)
			if err != nil {
				_, again := conn.ReadMessage(&dst, readLimit)
				assert.ErrorIs(t, again, err)
				break
			}
			assert.True(t, dst.Len() <= readLimit)
			if messageType == MessageText {
				assert.True(t, utf8.Valid(dst.Bytes()))
			}
		}
		assert.True(t, transport.closed)
	})
}

type fuzzTransport struct {
	io.Reader

	closed bool
}

func (*fuzzTransport) Write(data []byte) (int, error) {
	return len(data), nil
}

func (f *fuzzTransport) Close() error {
	f.closed = true
	return nil
}

// maskedFrame builds a final masked frame with a zero mask key.
func maskedFrame(op opcode, payload string) []byte {
	var space [MaxHeaderSize]byte
	start := putHeader(space[:], op, 0, len(payload), true)
	clear(space[MaxHeaderSize-4:])
	return append(space[start:], payload...)
}
