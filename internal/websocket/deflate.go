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
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
)

const (
	// deflateOffer is the one offer Dial makes.
	deflateOffer = "permessage-deflate; client_no_context_takeover; server_no_context_takeover"
	// deflateResponse is how Accept answers any acceptable offer. It imposes
	// no context takeover both ways, as RFC 008 Section 10 requires.
	deflateResponse = "permessage-deflate; server_no_context_takeover; client_no_context_takeover"
	// defaultCompressionThreshold is the smallest message WriteMessage
	// compresses unless Options say otherwise.
	defaultCompressionThreshold = 512
	// syncTail ends a flushed DEFLATE stream. The sender strips it, RFC 7692
	// Section 7.2.1.
	syncTail = "\x00\x00\xff\xff"
	// deflateTail follows every compressed message on receipt. It restores
	// syncTail, then adds an empty final stored block. flate then ends with
	// io.EOF, and input cut short fails instead of looking complete.
	deflateTail = syncTail + "\x01\x00\x00\xff\xff"
	// maxWindowBits is the only LZ77 window Go's flate supports.
	maxWindowBits = 15
	// maxRestarts bounds how often one message's DEFLATE stream restarts
	// after a BFINAL block. Each restart copies up to 32 KiB of history for
	// as little as 2 bytes of input, with or without a limit.
	maxRestarts = 16
	// maxRecycleBuffer keeps a huge message's buffer out of the pool.
	maxRecycleBuffer = 8 << 20
)

// Indexes of the permessage-deflate parameters, RFC 7692 Section 7.1.
const (
	paramServerNoContextTakeover = iota
	paramClientNoContextTakeover
	paramServerMaxWindowBits
	paramClientMaxWindowBits
	paramUnknown
)

var (
	//nolint:gochecknoglobals // package-level pool is the standard sync.Pool idiom
	compressorPool sync.Pool
	//nolint:gochecknoglobals // package-level pool is the standard sync.Pool idiom
	flateReaderPool sync.Pool
)

// extension is one element of a Sec-WebSocket-Extensions header.
type extension struct {
	name   string
	params []extensionParam
}

type extensionParam struct {
	name     string
	value    string
	hasValue bool
}

// extensionParser parses a Sec-WebSocket-Extensions value, RFC 6455 Section
// 9.1. Commas inside quoted strings don't split elements.
type extensionParser struct {
	input string
	pos   int
}

func (p *extensionParser) parse(extensions []extension) ([]extension, error) {
	for {
		p.skipSpace()
		if p.done() {
			return extensions, nil
		}
		if p.input[p.pos] == ',' {
			p.pos++
			continue
		}
		name, err := p.token()
		if err != nil {
			return nil, err
		}
		element := extension{name: name}
		for {
			p.skipSpace()
			if p.done() || p.input[p.pos] == ',' {
				break
			}
			if p.input[p.pos] != ';' {
				return nil, fmt.Errorf("unexpected %q at offset %d", p.input[p.pos], p.pos)
			}
			p.pos++
			p.skipSpace()
			param, err := p.param()
			if err != nil {
				return nil, err
			}
			element.params = append(element.params, param)
		}
		extensions = append(extensions, element)
	}
}

func (p *extensionParser) param() (extensionParam, error) {
	name, err := p.token()
	if err != nil {
		return extensionParam{}, err
	}
	p.skipSpace()
	if p.done() || p.input[p.pos] != '=' {
		return extensionParam{name: name}, nil
	}
	p.pos++
	p.skipSpace()
	var value string
	if !p.done() && p.input[p.pos] == '"' {
		value, err = p.quoted()
		if err == nil && !isToken(value) {
			err = fmt.Errorf("quoted value %q of %s is not a token", value, name)
		}
	} else {
		value, err = p.token()
	}
	return extensionParam{name: name, value: value, hasValue: true}, err
}

func (p *extensionParser) token() (string, error) {
	start := p.pos
	for !p.done() && isTokenChar(p.input[p.pos]) {
		p.pos++
	}
	if p.pos == start {
		return "", fmt.Errorf("expected a token at offset %d", start)
	}
	return p.input[start:p.pos], nil
}

// quoted reads a quoted-string and returns it unescaped.
func (p *extensionParser) quoted() (string, error) {
	start := p.pos
	p.pos++
	var value strings.Builder
	for !p.done() {
		char := p.input[p.pos]
		p.pos++
		switch {
		case char == '"':
			return value.String(), nil
		case char == '\\' && !p.done():
			char = p.input[p.pos]
			p.pos++
		case char < ' ' && char != '\t', char == 0x7f:
			return "", fmt.Errorf("control character in quoted string at offset %d", p.pos-1)
		}
		value.WriteByte(char)
	}
	return "", fmt.Errorf("unterminated quoted string at offset %d", start)
}

func (p *extensionParser) skipSpace() {
	for !p.done() && (p.input[p.pos] == ' ' || p.input[p.pos] == '\t') {
		p.pos++
	}
}

func (p *extensionParser) done() bool {
	return p.pos >= len(p.input)
}

// compressor deflates one message at a time. It keeps nothing between
// messages, which lets a pool share it between connections.
type compressor struct {
	writer *flate.Writer
	output bytes.Buffer
}

// compress deflates payload into a frame with MaxHeaderSize bytes of header
// space in front, as one stream of non-final blocks without the sync tail. It
// reports false when the result isn't smaller than payload. The output never
// sets BFINAL, which Safari fails on (WebKit bug 326651).
func (c *compressor) compress(payload []byte) ([]byte, bool) {
	var headerSpace [MaxHeaderSize]byte
	c.output.Reset()
	c.output.Write(headerSpace[:])
	c.writer.Reset(&c.output)
	// A bytes.Buffer never fails a write.
	_, _ = c.writer.Write(payload)
	_ = c.writer.Flush()
	frame := c.output.Bytes()
	end := len(frame) - len(syncTail)
	if end < MaxHeaderSize || string(frame[end:]) != syncTail {
		return nil, false
	}
	frame = frame[:end]
	return frame, len(frame)-MaxHeaderSize < len(payload)
}

// flateReader is what flate.NewReader returns.
type flateReader interface {
	io.Reader
	flate.Resetter
}

// inflater reads a compressed message's inflated bytes from a messageReader.
// It bounds them by the read limit and validates text.
type inflater struct {
	source *messageReader
	reader flateReader
	dst    *bytes.Buffer
	start  int   // dst's length when the message started
	left   int64 // output bytes still allowed, negative for no limit
	// restart is set when the DEFLATE stream hit a BFINAL block before the
	// message ended.
	restart  bool
	restarts int
	text     bool
	utf8     utf8Validator
}

// Read stops one byte past the limit, which it never hands to dst. At a
// BFINAL block before the end of the message it returns what it has and
// restarts the stream on the next call, once dst holds the output it needs
// as history.
func (i *inflater) Read(data []byte) (int, error) {
	if i.restart {
		if err := i.restartStream(); err != nil {
			return 0, err
		}
	}
	if i.left >= 0 && int64(len(data))-1 > i.left {
		data = data[:i.left+1]
	}
	size, err := i.reader.Read(data)
	if i.left >= 0 {
		if i.left -= int64(size); i.left < 0 {
			return size - 1, fmt.Errorf("%w: limit is %d bytes", ErrMessageTooBig, i.source.limit)
		}
	}
	if i.text && !i.utf8.valid(data[:size]) {
		return size, ErrInvalidUTF8
	}
	if errors.Is(err, io.EOF) && !i.source.finished() {
		// Once the payload is used up only deflateTail is left, which
		// can't start a stream.
		more, moreErr := i.source.morePayload()
		if moreErr != nil {
			return size, moreErr
		}
		if more {
			i.restart = true
			err = nil
		}
	}
	return size, err
}

// restartStream continues after a BFINAL block. Blocks after it may refer
// back to this message's earlier output, RFC 7692 Section 7.2.1.
func (i *inflater) restartStream() error {
	i.restart = false
	if i.restarts++; i.restarts > maxRestarts {
		return fmt.Errorf("%w: over %d BFINAL blocks in one message", ErrMessageTooBig, maxRestarts)
	}
	output := i.dst.Bytes()[i.start:]
	return i.reader.Reset(i.source, output[max(0, len(output)-1<<maxWindowBits):])
}

// negotiateDeflate picks the first acceptable permessage-deflate offer and
// returns the response for it. A malformed header turns compression off.
func negotiateDeflate(offers []string) (string, bool) {
	extensions, err := parseExtensions(offers)
	if err != nil {
		return "", false
	}
	for _, offer := range extensions {
		if !strings.EqualFold(offer.name, "permessage-deflate") {
			continue
		}
		if response, ok := acceptDeflateOffer(offer.params); ok {
			return response, true
		}
	}
	return "", false
}

// acceptDeflateOffer applies RFC 7692 Section 7.1 to one offer. Go's flate
// always uses a 32 KiB window, which rules out a smaller
// server_max_window_bits. Any client_max_window_bits is fine, since without
// context takeover the inflater never needs more history than the message.
func acceptDeflateOffer(params []extensionParam) (string, bool) {
	var seen [paramUnknown]bool
	for _, param := range params {
		index := deflateParam(param.name)
		if index == paramUnknown || seen[index] {
			return "", false
		}
		seen[index] = true
		switch index {
		case paramServerNoContextTakeover, paramClientNoContextTakeover:
			if param.hasValue {
				return "", false
			}
		case paramServerMaxWindowBits:
			if bits, ok := windowBits(param); !ok || bits != maxWindowBits {
				return "", false
			}
		case paramClientMaxWindowBits:
			if _, ok := windowBits(param); param.hasValue && !ok {
				return "", false
			}
		}
	}
	if seen[paramServerMaxWindowBits] {
		// RFC 7692 Section 7.1.2.1 requires echoing it.
		return deflateResponse + "; server_max_window_bits=15", true
	}
	return deflateResponse, true
}

// verifyDeflateResponse checks the server's answer to deflateOffer and
// reports whether it enabled compression. A missing
// client_no_context_takeover is allowed here. The caller can read the
// response header and apply its own policy.
func verifyDeflateResponse(values []string) (bool, error) {
	extensions, err := parseExtensions(values)
	if err != nil {
		return false, fmt.Errorf("handshake response has malformed extensions %q: %w", strings.Join(values, ", "), err)
	}
	if len(extensions) == 0 {
		return false, nil
	}
	if len(extensions) > 1 || !strings.EqualFold(extensions[0].name, "permessage-deflate") {
		return false, fmt.Errorf("handshake response has unrequested extensions %q", strings.Join(values, ", "))
	}
	var seen [paramUnknown]bool
	for _, param := range extensions[0].params {
		index := deflateParam(param.name)
		switch {
		case index == paramUnknown:
			return false, fmt.Errorf("handshake response has unknown permessage-deflate parameter %q", param.name)
		case seen[index]:
			return false, fmt.Errorf("handshake response repeats permessage-deflate parameter %q", param.name)
		case index == paramClientMaxWindowBits:
			return false, errors.New("handshake response has client_max_window_bits, which wasn't offered")
		case index == paramServerMaxWindowBits:
			if _, ok := windowBits(param); !ok {
				return false, fmt.Errorf("handshake response has invalid server_max_window_bits %q", param.value)
			}
		case param.hasValue:
			return false, fmt.Errorf("handshake response has a value for %s", param.name)
		}
		seen[index] = true
	}
	if !seen[paramServerNoContextTakeover] {
		return false, errors.New("handshake response negotiates permessage-deflate without server_no_context_takeover")
	}
	return true, nil
}

func parseExtensions(values []string) ([]extension, error) {
	var extensions []extension
	for _, value := range values {
		parser := extensionParser{input: value}
		var err error
		if extensions, err = parser.parse(extensions); err != nil {
			return nil, err
		}
	}
	return extensions, nil
}

func deflateParam(name string) int {
	switch {
	case strings.EqualFold(name, "server_no_context_takeover"):
		return paramServerNoContextTakeover
	case strings.EqualFold(name, "client_no_context_takeover"):
		return paramClientNoContextTakeover
	case strings.EqualFold(name, "server_max_window_bits"):
		return paramServerMaxWindowBits
	case strings.EqualFold(name, "client_max_window_bits"):
		return paramClientMaxWindowBits
	default:
		return paramUnknown
	}
}

// windowBits parses a *_max_window_bits value, 8 to 15 without leading
// zeros (RFC 7692 Section 7.1.2).
func windowBits(param extensionParam) (int, bool) {
	value := param.value
	switch {
	case len(value) == 1 && (value[0] == '8' || value[0] == '9'):
		return int(value[0] - '0'), true
	case len(value) == 2 && value[0] == '1' && value[1] >= '0' && value[1] <= '5':
		return 10 + int(value[1]-'0'), true
	default:
		return 0, false
	}
}

// compressedLimit bounds a compressed message's payload for a read limit.
// Deflate never grows data by more than a few bytes per 64 KiB block, which
// leaves an honest message far inside twice its limit.
func compressedLimit(limit int64) int64 {
	if limit < 0 || limit > (math.MaxInt64-1<<10)/2 {
		return -1
	}
	return 2*limit + 1<<10
}

func getCompressor() *compressor {
	if pooled, ok := compressorPool.Get().(*compressor); ok {
		return pooled
	}
	pooled := &compressor{}
	// NewWriter fails only for an invalid level.
	pooled.writer, _ = flate.NewWriter(&pooled.output, flate.BestSpeed)
	return pooled
}

func putCompressor(pooled *compressor) {
	if pooled.output.Cap() > maxRecycleBuffer {
		pooled.output = bytes.Buffer{}
	}
	compressorPool.Put(pooled)
}

// getFlateReader returns a pooled reader of source. source implements
// io.ByteReader, which keeps flate from wrapping it in a bufio.Reader.
func getFlateReader(source *messageReader) flateReader {
	if pooled, ok := flateReaderPool.Get().(flateReader); ok && pooled.Reset(source, nil) == nil {
		return pooled
	}
	// flate.NewReader always returns a Resetter.
	reader, _ := flate.NewReader(source).(flateReader)
	return reader
}

func isToken(value string) bool {
	for i := range len(value) {
		if !isTokenChar(value[i]) {
			return false
		}
	}
	return value != ""
}

// isTokenChar reports a tchar, RFC 9110 Section 5.6.2.
func isTokenChar(char byte) bool {
	switch {
	case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		return true
	default:
		return strings.IndexByte("!#$%&'*+-.^_`|~", char) >= 0
	}
}
