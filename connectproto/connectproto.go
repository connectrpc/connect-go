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

// Package connectproto provides protobuf codecs for [connect].
//
// [BinaryCodec] encodes messages using the binary protobuf wire format and is
// the default codec for [connectrpc.com/connect/v2/connecthttp] transports.
//
// [JSONCodec] encodes messages using the canonical protobuf JSON mapping
// and is registered alongside the binary codec by default.
package connectproto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/bufferpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// ErrNotProtoMessage is returned when a value passed to [BinaryCodec] or
// [JSONCodec] is not a [proto.Message].
var ErrNotProtoMessage = errors.New("connectproto: value does not implement proto.Message")

// Compile-time interface checks.
var (
	_ connect.Codec       = (*BinaryCodec)(nil)
	_ connect.StableCodec = (*BinaryCodec)(nil)
	_ connect.Codec       = (*JSONCodec)(nil)
	_ connect.StableCodec = (*JSONCodec)(nil)
)

// Option configures [NewBinaryCodec] and [NewJSONCodec]. Other protobuf
// settings are available through the exported fields on the returned codec.
type Option interface {
	apply(*options)
}

// TypeResolver resolves protobuf message and extension types.
type TypeResolver interface {
	protoregistry.MessageTypeResolver
	protoregistry.ExtensionTypeResolver
}

// WithTypeResolver sets the protobuf type resolver used for messages and
// extensions. Passing nil uses the default global registry.
func WithTypeResolver(res TypeResolver) Option {
	return optionFunc(func(o *options) { o.resolver = res })
}

// BinaryCodec encodes protobuf messages in the binary protobuf wire format.
//
// BinaryCodec implements [connect.Codec] and [connect.StableCodec]. Configure
// the exported option fields before concurrent use. Do not mutate them while
// marshaling or unmarshaling.
type BinaryCodec struct {
	// MarshalOptions configures binary protobuf marshaling.
	MarshalOptions proto.MarshalOptions
	// UnmarshalOptions configures binary protobuf unmarshaling.
	UnmarshalOptions proto.UnmarshalOptions
}

// NewBinaryCodec returns a binary protobuf codec. [WithTypeResolver] sets the
// resolver on [BinaryCodec.UnmarshalOptions]. Other settings can be configured
// by mutating the exported fields before use.
func NewBinaryCodec(opts ...Option) *BinaryCodec {
	var o options
	for _, opt := range opts {
		opt.apply(&o)
	}
	return &BinaryCodec{
		UnmarshalOptions: proto.UnmarshalOptions{Resolver: o.resolver},
	}
}

// Name returns "proto".
func (c *BinaryCodec) Name() string { return connect.CodecNameProto }

// IsBinary reports true.
func (c *BinaryCodec) IsBinary() bool { return true }

// MarshalWrite writes msg encoded as binary protobuf to dst.
func (c *BinaryCodec) MarshalWrite(_ context.Context, dst io.Writer, msg any) error {
	return c.marshalBinary(dst, msg, false /* deterministic */)
}

// MarshalWriteStable writes msg encoded as deterministic binary protobuf
// to dst.
func (c *BinaryCodec) MarshalWriteStable(_ context.Context, dst io.Writer, msg any) error {
	return c.marshalBinary(dst, msg, true /* deterministic */)
}

// UnmarshalRead decodes binary protobuf from src into msg. The payload is
// buffered into a pooled buffer before decoding. Protobuf binary decoding
// needs the whole message.
func (c *BinaryCodec) UnmarshalRead(_ context.Context, src io.Reader, msg any) error {
	protoMsg, ok := msg.(proto.Message)
	if !ok {
		return fmt.Errorf("%w: got %T", ErrNotProtoMessage, msg)
	}
	return unmarshalReadAll(src, func(data []byte) error {
		return c.UnmarshalOptions.Unmarshal(data, protoMsg)
	})
}

// marshalBinary encodes msg to dst. It sizes the message once and reuses
// that cached size for the in-place append, so encoding is a single pass.
func (c *BinaryCodec) marshalBinary(dst io.Writer, msg any, deterministic bool) error {
	protoMsg, ok := msg.(proto.Message)
	if !ok {
		return fmt.Errorf("%w: got %T", ErrNotProtoMessage, msg)
	}
	opts := c.MarshalOptions
	if deterministic {
		opts.Deterministic = deterministic
	}
	size := opts.Size(protoMsg)
	opts.UseCachedSize = true
	return marshalAppendWrite(dst, size, func(buf []byte) ([]byte, error) {
		return opts.MarshalAppend(buf, protoMsg)
	})
}

// JSONCodec encodes protobuf messages using the canonical protobuf JSON
// mapping.
//
// JSONCodec implements [connect.Codec] and [connect.StableCodec]. Configure
// the exported option fields before concurrent use. Do not mutate them while
// marshaling or unmarshaling.
type JSONCodec struct {
	// MarshalOptions configures protobuf JSON marshaling.
	MarshalOptions protojson.MarshalOptions
	// UnmarshalOptions configures protobuf JSON unmarshaling.
	UnmarshalOptions protojson.UnmarshalOptions
}

// NewJSONCodec returns a protobuf JSON codec. [WithTypeResolver] sets the
// resolver on both [JSONCodec.MarshalOptions] and [JSONCodec.UnmarshalOptions].
// Other settings can be configured by mutating the exported fields before use.
func NewJSONCodec(opts ...Option) *JSONCodec {
	var resolved options
	for _, opt := range opts {
		opt.apply(&resolved)
	}
	return &JSONCodec{
		MarshalOptions: protojson.MarshalOptions{Resolver: resolved.resolver},
		UnmarshalOptions: protojson.UnmarshalOptions{
			Resolver:       resolved.resolver,
			DiscardUnknown: true, // Ensure difference schema versions unmarshal.
		},
	}
}

// Name returns "json".
func (c *JSONCodec) Name() string { return connect.CodecNameJSON }

// IsBinary reports false.
func (c *JSONCodec) IsBinary() bool { return false }

// MarshalWrite writes msg encoded as protobuf JSON to dst.
func (c *JSONCodec) MarshalWrite(_ context.Context, dst io.Writer, msg any) error {
	protoMsg, ok := msg.(proto.Message)
	if !ok {
		return fmt.Errorf("%w: got %T", ErrNotProtoMessage, msg)
	}
	// protojson cannot size ahead of encoding, so pass no size hint.
	return marshalAppendWrite(dst, -1, func(buf []byte) ([]byte, error) {
		return c.MarshalOptions.MarshalAppend(buf, protoMsg)
	})
}

// MarshalWriteStable writes msg encoded as compact protobuf JSON to dst.
// protojson's whitespace is nondeterministic, so this method compacts the output
// so equivalent messages produce identical bytes for Connect GET request
// caching. protojson emits fields in field-number order, but map entries are
// not sorted, so messages containing maps are not guaranteed to be stable.
func (c *JSONCodec) MarshalWriteStable(_ context.Context, dst io.Writer, msg any) error {
	protoMsg, ok := msg.(proto.Message)
	if !ok {
		return fmt.Errorf("%w: got %T", ErrNotProtoMessage, msg)
	}
	raw, err := c.MarshalOptions.Marshal(protoMsg)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.Grow(len(raw))
	if err := json.Compact(&buf, raw); err != nil {
		return err
	}
	_, err = dst.Write(buf.Bytes())
	return err
}

// UnmarshalRead decodes protobuf JSON from src into msg. The payload is
// buffered into a pooled buffer before decoding. protojson does not yet
// support incremental input.
func (c *JSONCodec) UnmarshalRead(_ context.Context, src io.Reader, msg any) error {
	protoMsg, ok := msg.(proto.Message)
	if !ok {
		return fmt.Errorf("%w: got %T", ErrNotProtoMessage, msg)
	}
	return unmarshalReadAll(src, func(data []byte) error {
		if len(data) == 0 {
			// protojson rejects empty input with a low-level syntax error;
			// match the v1 codec's clearer message.
			return errors.New("zero-length payload is not a valid JSON object")
		}
		return c.UnmarshalOptions.Unmarshal(data, protoMsg)
	})
}

type options struct {
	resolver TypeResolver
}

type optionFunc func(*options)

func (f optionFunc) apply(o *options) { f(o) }

// availableBufferWriter is the in-place append fast path shared by
// [bytes.Buffer] and the writers connect transports pass to
// [connect.Codec.MarshalWrite]. Appending to AvailableBuffer and
// committing the result with Write encodes straight into the
// destination's spare capacity, skipping the staging buffer.
type availableBufferWriter interface {
	io.Writer
	AvailableBuffer() []byte
}

// grower is implemented by [bytes.Buffer] and transport writers that can
// reserve capacity up front. Growing to the exact encoded size before an
// append-style marshal keeps the marshal from reallocating, which would
// break the AvailableBuffer aliasing and force Write to copy.
type grower interface {
	Grow(n int)
}

// marshalAppendWrite writes the output of an append-style marshal
// function to dst, encoding in place when dst supports it. sizeHint, if
// non-negative, is the exact encoded size. It pre-grows dst so the
// in-place fast path is a single allocation even on a cold buffer.
func marshalAppendWrite(dst io.Writer, sizeHint int, marshalAppend func(dst []byte) ([]byte, error)) error {
	if buffered, ok := dst.(availableBufferWriter); ok {
		if grower, ok := dst.(grower); ok && sizeHint >= 0 {
			grower.Grow(sizeHint)
		}
		out, err := marshalAppend(buffered.AvailableBuffer())
		if err != nil {
			return err
		}
		_, err = buffered.Write(out)
		return err
	}
	buf := bufferpool.Get()
	defer bufferpool.Put(buf)
	if sizeHint >= 0 {
		buf.Grow(sizeHint)
	}
	out, err := marshalAppend(buf.AvailableBuffer())
	if err != nil {
		return err
	}
	_, err = dst.Write(out)
	return err
}

// unmarshalReadAll buffers src into a pooled buffer and decodes it with
// unmarshal. Read errors from src are returned as-is so typed transport
// errors (size caps, cancellation) keep their identity.
func unmarshalReadAll(src io.Reader, unmarshal func(data []byte) error) error {
	buf := bufferpool.Get()
	defer bufferpool.Put(buf)
	if _, err := buf.ReadFrom(src); err != nil {
		return err
	}
	return unmarshal(buf.Bytes())
}
