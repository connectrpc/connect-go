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
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

const (
	opContinuation opcode = 0
	opText         opcode = 1
	opBinary       opcode = 2
	opClose        opcode = 8
	opPing         opcode = 9
	opPong         opcode = 10

	maxControlPayload = 125
)

// opcode is a frame opcode from RFC 6455 Section 5.2.
type opcode byte

func (op opcode) isControl() bool {
	return op&0x8 != 0
}

// header is a frame header from RFC 6455 Section 5.2.
type header struct {
	fin     bool
	rsv     byte
	opcode  opcode
	masked  bool
	maskKey [4]byte
	// length is negative when the 64-bit length has its top bit set.
	length int64
}

// check reports a header that fails the connection with 1002. A client
// receives unmasked frames and a server masked ones.
func (h *header) check(client bool) error {
	switch {
	case h.rsv != 0:
		return fmt.Errorf("frame has reserved bits 0x%02x set", h.rsv)
	case h.opcode > opBinary && h.opcode < opClose, h.opcode > opPong:
		return fmt.Errorf("frame has unknown opcode %d", h.opcode)
	case client && h.masked:
		return errors.New("masked frame from server")
	case !client && !h.masked:
		return errors.New("unmasked frame from client")
	case h.length < 0:
		return errors.New("frame length has the most significant bit set")
	case h.opcode.isControl() && !h.fin:
		return errors.New("fragmented control frame")
	case h.opcode.isControl() && h.length > maxControlPayload:
		return fmt.Errorf("control frame payload of %d bytes exceeds %d", h.length, maxControlPayload)
	}
	return nil
}

// utf8Validator checks text that arrives in pieces. It holds back the bytes
// of a rune split between pieces.
type utf8Validator struct {
	partial [utf8.UTFMax]byte
	size    int
}

func (v *utf8Validator) valid(data []byte) bool {
	for v.size > 0 && len(data) > 0 {
		v.partial[v.size] = data[0]
		v.size++
		data = data[1:]
		if utf8.FullRune(v.partial[:v.size]) {
			if r, size := utf8.DecodeRune(v.partial[:v.size]); r == utf8.RuneError && size <= 1 {
				return false
			}
			v.size = 0
		}
	}
	end := len(data)
	for i := len(data) - 1; i >= 0 && i > len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				end = i
			}
			break
		}
	}
	if !utf8.Valid(data[:end]) {
		return false
	}
	v.size += copy(v.partial[v.size:], data[end:])
	return true
}

// complete reports whether no rune is left unfinished.
func (v *utf8Validator) complete() bool {
	return v.size == 0
}

// readHeader reads one frame header. It reads byte by byte, which a
// bufio.Reader makes cheap, to avoid allocating.
func readHeader(reader *bufio.Reader) (header, error) {
	var hdr header
	first, err := reader.ReadByte()
	if err != nil {
		return hdr, err
	}
	second, err := reader.ReadByte()
	if err != nil {
		return hdr, err
	}
	hdr.fin = first&0x80 != 0
	hdr.rsv = first & 0x70
	hdr.opcode = opcode(first & 0x0f)
	hdr.masked = second&0x80 != 0
	var size int
	switch length := second & 0x7f; length {
	case 126:
		size = 2
	case 127:
		size = 8
	default:
		hdr.length = int64(length)
	}
	if size > 0 {
		var value uint64
		for range size {
			next, err := reader.ReadByte()
			if err != nil {
				return hdr, err
			}
			value = value<<8 | uint64(next)
		}
		hdr.length = int64(value) // check rejects a negative length
	}
	if hdr.masked {
		for i := range hdr.maskKey {
			if hdr.maskKey[i], err = reader.ReadByte(); err != nil {
				return hdr, err
			}
		}
	}
	return hdr, nil
}

// putHeader writes a final frame's header right-aligned into
// space[:MaxHeaderSize] and returns where it starts. A client header ends
// with a fresh mask key.
func putHeader(space []byte, frameOpcode opcode, length int, client bool) int {
	end := MaxHeaderSize
	var maskBit byte
	if client {
		end -= 4
		maskBit = 0x80
		_, _ = rand.Read(space[end:MaxHeaderSize]) // never fails, see crypto/rand.Read
	}
	var start int
	switch {
	case length <= maxControlPayload:
		start = end - 2
		space[start+1] = maskBit | byte(length) //nolint:gosec // at most 125
	case length <= math.MaxUint16:
		start = end - 4
		space[start+1] = maskBit | 126
		binary.BigEndian.PutUint16(space[start+2:], uint16(length))
	default:
		start = end - 10
		space[start+1] = maskBit | 127
		binary.BigEndian.PutUint64(space[start+2:], uint64(length))
	}
	space[start] = 0x80 | byte(frameOpcode)
	return start
}

// mask XORs data with key, starting pos bytes into the key, and returns the
// position for the next call. RFC 6455 Section 5.3.
func mask(data []byte, key [4]byte, pos int) int {
	if len(data) >= 8 {
		var wide [8]byte
		for i := range wide {
			wide[i] = key[(pos+i)&3]
		}
		key64 := binary.LittleEndian.Uint64(wide[:])
		for len(data) >= 8 {
			binary.LittleEndian.PutUint64(data, binary.LittleEndian.Uint64(data)^key64)
			data = data[8:]
		}
	}
	for i := range data {
		data[i] ^= key[pos&3]
		pos++
	}
	return pos & 3
}

// parseClose validates a Close frame's payload. On failure it returns the
// code to fail the connection with.
func parseClose(payload []byte) (*CloseError, StatusCode, error) {
	switch len(payload) {
	case 0:
		return &CloseError{Code: StatusNoStatusReceived}, 0, nil
	case 1:
		return nil, StatusProtocolError, fmt.Errorf("%w: close frame payload of 1 byte", ErrProtocol)
	}
	code := StatusCode(binary.BigEndian.Uint16(payload))
	if !validCloseCode(code) {
		return nil, StatusProtocolError, fmt.Errorf("%w: invalid close code %d", ErrProtocol, code)
	}
	if !utf8.Valid(payload[2:]) {
		return nil, StatusInvalidPayloadData, fmt.Errorf("%w: close reason", ErrInvalidUTF8)
	}
	return &CloseError{Code: code, Reason: string(payload[2:])}, 0, nil
}
