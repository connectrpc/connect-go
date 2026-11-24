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
	// The longest header has a 64-bit length and a mask key.
	maxHeaderSize = 14
)

var errFrameLength = errors.New("frame length has the most significant bit set")

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
	length  int64
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
	switch length := second & 0x7f; length {
	case 126:
		value, err := readUint(reader, 2)
		if err != nil {
			return hdr, err
		}
		hdr.length = int64(value) //nolint:gosec // two bytes
	case 127:
		value, err := readUint(reader, 8)
		if err != nil {
			return hdr, err
		}
		if value > math.MaxInt64 {
			return hdr, errFrameLength
		}
		hdr.length = int64(value)
	default:
		hdr.length = int64(length)
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

func readUint(reader *bufio.Reader, size int) (uint64, error) {
	var value uint64
	for range size {
		next, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}
		value = value<<8 | uint64(next)
	}
	return value, nil
}

// appendHeader appends a final frame's header with the shortest length
// encoding. A nil maskKey leaves the frame unmasked.
func appendHeader(dst []byte, op opcode, length int, maskKey *[4]byte) []byte {
	dst = append(dst, 0x80|byte(op))
	var maskBit byte
	if maskKey != nil {
		maskBit = 0x80
	}
	switch {
	case length <= maxControlPayload:
		dst = append(dst, maskBit|byte(length)) //nolint:gosec // at most 125
	case length <= math.MaxUint16:
		dst = append(dst, maskBit|126)
		dst = binary.BigEndian.AppendUint16(dst, uint16(length))
	default:
		dst = append(dst, maskBit|127)
		dst = binary.BigEndian.AppendUint64(dst, uint64(length))
	}
	if maskKey != nil {
		dst = append(dst, maskKey[:]...)
	}
	return dst
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
