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

// Package envelope implements the Enveloped-Message framing shared by the
// Connect, gRPC, and gRPC-Web protocols: a 5-byte prefix of flags and length
// followed by the payload.
//
// The framing is a protocol concept rather than an HTTP one, so transports
// other than net/http reuse it.
package envelope

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/bufferpool"
	"connectrpc.com/connect/v2/internal/compression"
	"connectrpc.com/connect/v2/internal/connecterr"
)

// FlagCompressed indicates that the data is compressed. It has the same
// meaning in the gRPC-Web, gRPC-HTTP2, and Connect protocols.
const FlagCompressed = 0b00000001

// discardLimit bounds how much data we are willing to throw away when
// draining a stream.
const discardLimit = 1024 * 1024 * 4 // 4MiB

// ErrSpecialEnvelope reports that the final message carried protocol-specific
// flags. User code checks for end of stream with errors.Is(err, io.EOF).
var ErrSpecialEnvelope = connect.Errorf(
	connect.CodeUnknown,
	"final message has protocol-specific flags: %s",
	io.EOF,
).WithCause(io.EOF)

// Envelope is a block of arbitrary bytes wrapped in gRPC and Connect's framing
// protocol.
//
// Each message is preceded by a 5-byte prefix. The first byte is a uint8 used
// as a set of bitwise flags, and the remainder is a uint32 indicating the
// message length. gRPC and Connect interpret the bitwise flags differently, so
// Envelope leaves their interpretation up to the caller.
type Envelope struct {
	Data   *bytes.Buffer
	Flags  uint8
	offset int64
}

var _ MessagePayload = (*Envelope)(nil)

// IsSet reports whether flag is set on the envelope.
func (e *Envelope) IsSet(flag uint8) bool {
	return e.Flags&flag == flag
}

// Read implements [io.Reader].
func (e *Envelope) Read(data []byte) (readN int, err error) {
	if e.offset < 5 {
		prefix, err := makeEnvelopePrefix(e.Flags, e.Data.Len())
		if err != nil {
			return 0, err
		}
		readN = copy(data, prefix[e.offset:])
		e.offset += int64(readN)
		if e.offset < 5 {
			return readN, nil
		}
		data = data[readN:]
	}
	n := copy(data, e.Data.Bytes()[e.offset-5:])
	e.offset += int64(n)
	readN += n
	if readN == 0 && e.offset == int64(e.Data.Len()+5) {
		err = io.EOF
	}
	return readN, err
}

// WriteTo implements [io.WriterTo].
func (e *Envelope) WriteTo(dst io.Writer) (wroteN int64, err error) {
	if e.offset < 5 {
		prefix, err := makeEnvelopePrefix(e.Flags, e.Data.Len())
		if err != nil {
			return 0, err
		}
		prefixN, err := dst.Write(prefix[e.offset:])
		e.offset += int64(prefixN)
		wroteN += int64(prefixN)
		if e.offset < 5 {
			return wroteN, err
		}
	}
	n, err := dst.Write(e.Data.Bytes()[e.offset-5:])
	e.offset += int64(n)
	wroteN += int64(n)
	return wroteN, err
}

// Seek implements [io.Seeker]. Based on the implementation of [bytes.Reader].
func (e *Envelope) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = e.offset + offset
	case io.SeekEnd:
		abs = int64(e.Data.Len()) + offset
	default:
		return 0, errors.New("connect.envelope.Seek: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("connect.envelope.Seek: negative position")
	}
	e.offset = abs
	return abs, nil
}

// Len returns the number of bytes of the unread portion of the envelope.
func (e *Envelope) Len() int {
	if length := int(int64(e.Data.Len()) + 5 - e.offset); length > 0 {
		return length
	}
	return 0
}

// MessagePayload is a sized and seekable message payload. The interface is
// implemented by [*bytes.Reader] and [*Envelope]. Reads must be non-blocking.
type MessagePayload interface {
	io.Reader
	io.WriterTo
	io.Seeker
	Len() int
}

// NopPayload is a message payload that does nothing. It's used to send headers
// to the server.
type NopPayload struct{}

var _ MessagePayload = NopPayload{}

// Read implements [io.Reader].
func (NopPayload) Read([]byte) (int, error) {
	return 0, io.EOF
}

// WriteTo implements [io.WriterTo].
func (NopPayload) WriteTo(io.Writer) (int64, error) {
	return 0, nil
}

// Seek implements [io.Seeker].
func (NopPayload) Seek(int64, int) (int64, error) {
	return 0, nil
}

// Len implements [MessagePayload].
func (NopPayload) Len() int {
	return 0
}

// MessageSender sends a message payload. Transports implement it over
// whatever carries their bytes.
type MessageSender interface {
	Send(MessagePayload) (int64, error)
}

// Writer marshals messages into envelopes and hands them to a
// [MessageSender].
type Writer struct {
	Ctx              context.Context //nolint:containedctx
	Sender           MessageSender
	Codec            connect.Codec
	CompressMinBytes int
	CompressionPool  *compression.Pool
	SendMaxBytes     int
	Stats            *connect.MessageStats
}

// recordStats records message byte counts, skipping protocol envelopes.
func (w *Writer) recordStats(flags uint8, size, compressedSize int) {
	if w.Stats == nil || flags&^FlagCompressed != 0 {
		return
	}
	*w.Stats = connect.MessageStats{Size: size, CompressedSize: compressedSize}
}

// Marshal encodes message and writes it as one envelope. A nil message sends
// a no-op payload, which creates the request and flushes headers.
func (w *Writer) Marshal(message any) *connect.Error {
	if message == nil {
		payload := NopPayload{}
		if _, err := w.Sender.Send(payload); err != nil {
			if connectErr, ok := connecterr.AsError(err); ok {
				return connectErr
			}
			return connect.Errorf(connect.CodeUnknown, "%s", err).WithCause(err)
		}
		return nil
	}
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	if err := w.Codec.MarshalWrite(w.Ctx, buffer, message); err != nil {
		return connect.Errorf(connect.CodeInternal, "marshal message: %s", err).WithCause(err)
	}
	env := &Envelope{Data: buffer}
	return w.Write(env)
}

// Write writes the enveloped message, compressing as necessary. It doesn't
// retain any references to the supplied envelope or its underlying data.
func (w *Writer) Write(env *Envelope) *connect.Error {
	if env.IsSet(FlagCompressed) ||
		w.CompressionPool == nil ||
		env.Data.Len() < w.CompressMinBytes {
		if w.SendMaxBytes > 0 && env.Data.Len() > w.SendMaxBytes {
			return connect.Errorf(connect.CodeResourceExhausted, "message size %d exceeds sendMaxBytes %d", env.Data.Len(), w.SendMaxBytes)
		}
		size, compressedSize := env.Data.Len(), 0
		if env.IsSet(FlagCompressed) {
			// Pre-compressed payload; uncompressed size unknown.
			size, compressedSize = 0, env.Data.Len()
		}
		if err := w.write(env); err != nil {
			return err
		}
		w.recordStats(env.Flags, size, compressedSize)
		return nil
	}
	size := env.Data.Len() // before Compress drains the buffer
	data := bufferpool.Get()
	defer bufferpool.Put(data)
	if err := w.CompressionPool.Compress(data, env.Data); err != nil {
		return err
	}
	if w.SendMaxBytes > 0 && data.Len() > w.SendMaxBytes {
		return connect.Errorf(connect.CodeResourceExhausted, "compressed message size %d exceeds sendMaxBytes %d", data.Len(), w.SendMaxBytes)
	}
	compressedSize := data.Len() // before write drains the buffer
	if err := w.write(&Envelope{
		Data:  data,
		Flags: env.Flags | FlagCompressed,
	}); err != nil {
		return err
	}
	w.recordStats(env.Flags, size, compressedSize)
	return nil
}

func (w *Writer) write(env *Envelope) *connect.Error {
	if _, err := w.Sender.Send(env); err != nil {
		err = connecterr.WrapIfContextDone(w.Ctx, err)
		if connectErr, ok := connecterr.AsError(err); ok {
			return connectErr
		}
		return connect.Errorf(connect.CodeUnknown, "write envelope: %s", err).WithCause(err)
	}
	return nil
}

// Reader reads envelopes from Src and decodes their payloads.
type Reader struct {
	Ctx             context.Context //nolint:containedctx
	Src             io.Reader
	BytesRead       int64 // detect trailers-only gRPC responses
	Codec           connect.Codec
	Last            Envelope
	CompressionPool *compression.Pool
	ReadMaxBytes    int
	Stats           *connect.MessageStats
}

// Unmarshal reads one envelope and decodes its payload into message. It
// returns [ErrSpecialEnvelope] when the envelope carries protocol-specific
// flags, leaving the payload on Last for the caller to interpret.
func (r *Reader) Unmarshal(message any) *connect.Error {
	buffer := bufferpool.Get()
	var dontRelease *bytes.Buffer
	defer func() {
		if buffer != dontRelease {
			bufferpool.Put(buffer)
		}
	}()

	env := &Envelope{Data: buffer}
	err := r.Read(env)
	switch {
	case err == nil && env.IsSet(FlagCompressed) && r.CompressionPool == nil:
		return connect.Errorf(connect.CodeInternal,
			"protocol error: sent compressed message without compression support",
		)
	case err == nil &&
		(env.Flags == 0 || env.Flags == FlagCompressed) &&
		env.Data.Len() == 0:
		// This is a standard message (because none of the top 7 bits are set) and
		// there's no data, so the zero value of the message is correct.
		if r.Stats != nil {
			*r.Stats = connect.MessageStats{}
		}
		return nil
	case err != nil && errors.Is(err, io.EOF):
		// The stream has ended. Propagate the EOF to the caller.
		return err
	case err != nil:
		// Something's wrong.
		return err
	}

	data := env.Data
	compressedSize := 0
	if data.Len() > 0 && env.IsSet(FlagCompressed) {
		compressedSize = data.Len()
		decompressed := bufferpool.Get()
		defer func() {
			if decompressed != dontRelease {
				bufferpool.Put(decompressed)
			}
		}()
		if err := r.CompressionPool.Decompress(decompressed, data, int64(r.ReadMaxBytes)); err != nil {
			return err
		}
		data = decompressed
	}

	if env.Flags != 0 && env.Flags != FlagCompressed {
		// Drain the rest of the stream to ensure there is no extra data.
		numBytes, err := discard(r.Src)
		r.BytesRead += numBytes
		if err != nil {
			err = connecterr.WrapIfContextError(err)
			if connErr, ok := connecterr.AsError(err); ok {
				return connErr
			}
			return connect.Errorf(connect.CodeInternal, "corrupt response: I/O error after end-stream message: %s", err).WithCause(err)
		} else if numBytes > 0 {
			return connect.Errorf(connect.CodeInternal, "corrupt response: %d extra bytes after end of stream", numBytes)
		}
		// One of the protocol-specific flags are set, so this is the end of the
		// stream. Save the message for protocol-specific code to process and
		// return a sentinel error. We alias the buffer with dontRelease as a
		// way of marking it so above defers don't release it to the pool.
		r.Last = Envelope{
			Data:  data,
			Flags: env.Flags,
		}
		dontRelease = data
		return ErrSpecialEnvelope
	}

	size := data.Len() // before UnmarshalRead drains the buffer
	if err := r.Codec.UnmarshalRead(r.Ctx, data, message); err != nil {
		return connect.Errorf(connect.CodeInvalidArgument, "unmarshal message: %s", err).WithCause(err)
	}
	if r.Stats != nil {
		*r.Stats = connect.MessageStats{Size: size, CompressedSize: compressedSize}
	}
	return nil
}

// Read fills env with the next envelope from Src.
func (r *Reader) Read(env *Envelope) *connect.Error {
	prefixes := [5]byte{}
	// io.ReadFull reads the number of bytes requested, or returns an error.
	// io.EOF will only be returned if no bytes were read.
	n, err := io.ReadFull(r.Src, prefixes[:])
	r.BytesRead += int64(n)
	if err != nil {
		if errors.Is(err, io.EOF) {
			// The stream ended cleanly. That's expected, but we need to propagate an EOF
			// to the user so that they know that the stream has ended. We shouldn't
			// add any alarming text about protocol errors, though.
			return connect.Errorf(connect.CodeUnknown, "%s", err).WithCause(err)
		}
		err = connecterr.WrapIfMaxBytesError(err, "read 5 byte message prefix")
		err = connecterr.WrapIfContextDone(r.Ctx, err)
		if connectErr, ok := connecterr.AsError(err); ok {
			return connectErr
		}
		// Something else has gone wrong - the stream didn't end cleanly.
		return connect.Errorf(
			connect.CodeInvalidArgument,
			"protocol error: incomplete envelope: %s", err,
		).WithCause(err)
	}
	size := int64(binary.BigEndian.Uint32(prefixes[1:5]))
	if r.ReadMaxBytes > 0 && size > int64(r.ReadMaxBytes) {
		n, err := io.CopyN(io.Discard, r.Src, size)
		r.BytesRead += n
		if err != nil && !errors.Is(err, io.EOF) {
			return connect.Errorf(connect.CodeResourceExhausted, "message is larger than configured max %d - unable to determine message size: %s", r.ReadMaxBytes, err).WithCause(err)
		}
		return connect.Errorf(connect.CodeResourceExhausted, "message size %d is larger than configured max %d", size, r.ReadMaxBytes)
	}
	// We've read the prefix, so we know how many bytes to expect.
	// CopyN will return an error if it doesn't read the requested
	// number of bytes.
	readN, err := io.CopyN(env.Data, r.Src, size)
	r.BytesRead += readN
	if err != nil {
		if errors.Is(err, io.EOF) {
			// We've gotten fewer bytes than we expected, so the stream has ended
			// unexpectedly.
			return connect.Errorf(connect.CodeInvalidArgument,
				"protocol error: promised %d bytes in enveloped message, got %d bytes",
				size,
				readN,
			)
		}
		err = connecterr.WrapIfMaxBytesError(err, "read %d byte message", size)
		err = connecterr.WrapIfContextDone(r.Ctx, err)
		if connectErr, ok := connecterr.AsError(err); ok {
			return connectErr
		}
		return connect.Errorf(connect.CodeUnknown, "read enveloped message: %s", err).WithCause(err)
	}
	env.Flags = prefixes[0]
	return nil
}

func makeEnvelopePrefix(flags uint8, size int) ([5]byte, error) {
	// Cast as 64-bit to ensure comparison works on all architectures.
	size64 := int64(size)
	if size64 < 0 || size64 > math.MaxUint32 {
		return [5]byte{}, fmt.Errorf("connect.makeEnvelopePrefix: size %d out of bounds", size)
	}
	prefix := [5]byte{}
	prefix[0] = flags
	binary.BigEndian.PutUint32(prefix[1:5], uint32(size64))
	return prefix, nil
}

// discard drains reader, giving up after discardLimit bytes so a misbehaving
// peer cannot make us read forever.
func discard(reader io.Reader) (int64, error) {
	if lr, ok := reader.(*io.LimitedReader); ok {
		return io.Copy(io.Discard, lr)
	}
	lr := &io.LimitedReader{R: reader, N: discardLimit}
	return io.Copy(io.Discard, lr)
}
