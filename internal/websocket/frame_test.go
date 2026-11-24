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
	"context"
	"io"
	"testing"
	"unicode/utf8"

	"connectrpc.com/connect/v2/internal/assert"
)

func TestHeaderRoundTrip(t *testing.T) {
	t.Parallel()
	for _, length := range []int{0, 1, 125, 126, 65535, 65536, 1 << 20} {
		for _, masked := range []bool{false, true} {
			var key *[4]byte
			if masked {
				key = &[4]byte{1, 2, 3, 4}
			}
			encoded := appendHeader(nil, opBinary, length, key)
			wantSize := 2
			switch {
			case length > 65535:
				wantSize += 8
			case length > 125:
				wantSize += 2
			}
			if masked {
				wantSize += 4
			}
			assert.Equal(t, len(encoded), wantSize, assert.Sprintf("length %d masked %t", length, masked))
			hdr, err := readHeader(bufio.NewReader(bytes.NewReader(encoded)))
			assert.Nil(t, err)
			assert.True(t, hdr.fin)
			assert.Equal(t, hdr.rsv, 0)
			assert.Equal(t, hdr.opcode, opBinary)
			assert.Equal(t, hdr.length, int64(length))
			assert.Equal(t, hdr.masked, masked)
			if masked {
				assert.Equal(t, hdr.maskKey, *key)
			}
		}
	}
}

func TestReadHeader(t *testing.T) {
	t.Parallel()
	t.Run("length_msb", func(t *testing.T) {
		t.Parallel()
		_, err := readHeader(bufio.NewReader(bytes.NewReader([]byte{0x82, 0x7f, 0x80, 0, 0, 0, 0, 0, 0, 0})))
		assert.ErrorIs(t, err, errFrameLength)
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
		name   string
		hdr    header
		client bool
		valid  bool
	}{
		{name: "text", hdr: header{fin: true, opcode: opText, masked: true}, valid: true},
		{name: "client_unmasked", hdr: header{fin: true, opcode: opText}, client: true, valid: true},
		{name: "unmasked_from_client", hdr: header{fin: true, opcode: opText}},
		{name: "masked_from_server", hdr: header{fin: true, opcode: opText, masked: true}, client: true},
		{name: "rsv1", hdr: header{fin: true, rsv: 0x40, opcode: opText, masked: true}},
		{name: "rsv3", hdr: header{fin: true, rsv: 0x10, opcode: opText, masked: true}},
		{name: "opcode_3", hdr: header{fin: true, opcode: 3, masked: true}},
		{name: "opcode_11", hdr: header{fin: true, opcode: 11, masked: true}},
		{name: "fragmented_ping", hdr: header{opcode: opPing, masked: true}},
		{name: "ping_125", hdr: header{fin: true, opcode: opPing, masked: true, length: 125}, valid: true},
		{name: "ping_126", hdr: header{fin: true, opcode: opPing, masked: true, length: 126}},
		{name: "fragmented_text", hdr: header{opcode: opText, masked: true, length: 1 << 40}, valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.hdr.check(test.client)
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

// FuzzReadFrame reads arbitrary bytes as frames from a peer. Reads must end
// in an error without panicking, and every message must respect the read
// limit and UTF-8 rules.
func FuzzReadFrame(f *testing.F) {
	const readLimit = 64
	f.Add(false, appendHeader(nil, opText, 2, &[4]byte{}))
	f.Add(true, append(appendHeader(nil, opBinary, 3, nil), "abc"...))
	f.Add(true, []byte{0x01, 0x01, 'a', 0x89, 0x00, 0x80, 0x01, 'b', 0x88, 0x02, 0x03, 0xe8})
	f.Add(true, []byte{0x82, 0x7f, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff})
	f.Add(false, []byte{0x88, 0x82, 0, 0, 0, 0, 0x03, 0xe8})
	f.Fuzz(func(t *testing.T, client bool, data []byte) {
		transport := &fuzzTransport{Reader: bytes.NewReader(data)}
		conn := newConn(transport, bufio.NewReader(transport), bufio.NewWriter(io.Discard), client)
		conn.SetReadLimit(readLimit)
		for {
			messageType, message, err := conn.Read(context.Background())
			if err != nil {
				break
			}
			assert.True(t, len(message) <= readLimit)
			if messageType == MessageText {
				assert.True(t, utf8.Valid(message))
			}
		}
		_ = conn.CloseNow()
	})
}

type fuzzTransport struct {
	io.Reader
}

func (*fuzzTransport) Write(data []byte) (int, error) {
	return len(data), nil
}

func (*fuzzTransport) Close() error {
	return nil
}
