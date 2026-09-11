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

package internal

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// StreamDecoder is used to decode messages from a stream. This is used
// when the input contains a sequence of messages, not just one.
type StreamDecoder interface {
	DecodeNext(msg proto.Message) error
}

// StreamEncoder is used to encode messages to a stream. This is used
// when the output will contain a sequence of messages, not just one.
type StreamEncoder interface {
	Encode(msg proto.Message) error
}

// Codec describes anything that can marshal and unmarshal proto messages.
type Codec interface {
	NewDecoder(io.Reader) StreamDecoder
	NewEncoder(io.Writer) StreamEncoder
}

// NewCodec returns a new Codec.
func NewCodec(json bool) Codec {
	if json {
		return &jsonCodec{MarshalOptions: protojson.MarshalOptions{Multiline: true}}
	}
	return &protoCodec{}
}

// jsonCodec marshals and unmarshals the JSON format.
type jsonCodec struct {
	protojson.MarshalOptions
	protojson.UnmarshalOptions
}

func (c *jsonCodec) NewDecoder(in io.Reader) StreamDecoder {
	dec := json.NewDecoder(in)
	return &jsonDecoder{
		opts:    c.UnmarshalOptions,
		decoder: dec,
	}
}

func (c *jsonCodec) NewEncoder(out io.Writer) StreamEncoder {
	return &jsonEncoder{
		opts: c.MarshalOptions,
		out:  out,
	}
}

type jsonDecoder struct {
	opts    protojson.UnmarshalOptions
	decoder *json.Decoder
}

func (j *jsonDecoder) DecodeNext(msg proto.Message) error {
	var msgData json.RawMessage
	if err := j.decoder.Decode(&msgData); err != nil {
		if errors.Is(err, io.EOF) {
			return err
		}
		return fmt.Errorf("failed to decode JSON message from input: %w", err)
	}
	if err := j.opts.Unmarshal(msgData, msg); err != nil {
		return fmt.Errorf("failed to unmarshal JSON message: %w", err)
	}
	return nil
}

type jsonEncoder struct {
	opts protojson.MarshalOptions
	out  io.Writer
}

func (j *jsonEncoder) Encode(msg proto.Message) error {
	data, err := j.opts.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message to JSON: %w", err)
	}
	if _, err := j.out.Write(data); err != nil {
		return fmt.Errorf("failed to write message to output: %w", err)
	}
	if len(data) > 0 || data[len(data)-1] != '\n' {
		_, _ = j.out.Write([]byte{'\n'}) // best effort newline between JSON outputs
	}
	return nil
}

// protoCodec marshals and unmarshals the Protobuf binary format.
type protoCodec struct {
	proto.MarshalOptions
	proto.UnmarshalOptions
}

func (c *protoCodec) NewDecoder(in io.Reader) StreamDecoder {
	return &protoDecoder{
		opts: c.UnmarshalOptions,
		in:   in,
	}
}

func (c *protoCodec) NewEncoder(out io.Writer) StreamEncoder {
	return &protoEncoder{
		opts: c.MarshalOptions,
		out:  out,
	}
}

type protoDecoder struct {
	opts proto.UnmarshalOptions
	in   io.Reader
}

func (p *protoDecoder) DecodeNext(msg proto.Message) error {
	var lenBuffer [4]byte
	if _, err := io.ReadFull(p.in, lenBuffer[:]); err != nil {
		return err
	}
	data := make([]byte, binary.BigEndian.Uint32(lenBuffer[:]))
	if _, err := io.ReadFull(p.in, data); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if err := p.opts.Unmarshal(data, msg); err != nil {
		return fmt.Errorf("failed to unmarshal binary message: %w", err)
	}
	return nil
}

type protoEncoder struct {
	opts proto.MarshalOptions
	out  io.Writer
}

func (p *protoEncoder) Encode(msg proto.Message) error {
	data, err := p.opts.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal response to binary: %w", err)
	}
	return writeDelimitedMessageRaw(p.out, data)
}

// NewStrictJSONCodec returns a JSON [connect.Codec] that rejects unrecognized
// fields. Connect's builtin JSON codec discards unknown fields, but the
// conformance suite needs the stricter behavior, so we build on connectproto
// and turn DiscardUnknown off.
func NewStrictJSONCodec() connect.Codec {
	codec := connectproto.NewJSONCodec()
	codec.UnmarshalOptions.DiscardUnknown = false
	return codec
}
