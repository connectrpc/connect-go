// Copyright 2021-2022 Buf Technologies, Inc.
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
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	codecNameProto = "proto"
	codecNameJSON  = "json"
)

// A Codec can marshal structs (typically generated from a schema) to and from
// bytes.
type Codec interface {
	Name() string
	Marshal(any) ([]byte, error)
	Unmarshal([]byte, any) error
}

type protoBinaryCodec struct{}

var _ Codec = (*protoBinaryCodec)(nil)

func (c *protoBinaryCodec) Name() string { return codecNameProto }

func (c *protoBinaryCodec) Marshal(message any) ([]byte, error) {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return nil, errNotProto(message)
	}
	return proto.Marshal(protoMessage)
}

func (c *protoBinaryCodec) Unmarshal(bs []byte, message any) error {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return errNotProto(message)
	}
	return proto.Unmarshal(bs, protoMessage)
}

type protoJSONCodec struct {
	marshalOptions   protojson.MarshalOptions
	unmarshalOptions protojson.UnmarshalOptions
}

var _ Codec = (*protoJSONCodec)(nil)

func (c *protoJSONCodec) Name() string { return codecNameJSON }

func (c *protoJSONCodec) Marshal(message any) ([]byte, error) {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return nil, errNotProto(message)
	}
	return c.marshalOptions.Marshal(protoMessage)
}

func (c *protoJSONCodec) Unmarshal(bs []byte, message any) error {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return errNotProto(message)
	}
	return c.unmarshalOptions.Unmarshal(bs, protoMessage)
}

func errNotProto(m any) error {
	return fmt.Errorf("%T doesn't implement proto.Message", m)
}

// readOnlyCodecs is a read-only interface to a map of named codecs.
type readOnlyCodecs interface {
	Get(string) Codec
	Protobuf() Codec
	Names() []string
}

type codecMap struct {
	codecs map[string]Codec
}

func newReadOnlyCodecs(m map[string]Codec) *codecMap {
	return &codecMap{m}
}

// Get the named codec.
func (m *codecMap) Get(name string) Codec {
	return m.codecs[name]
}

// Get the user-supplied protobuf codec, falling back to the default
// implementation if necessary.
//
// This is helpful in the gRPC protocol, where the wire protocol requires
// marshaling protobuf structs to binary even if the RPC procedures were
// generated from a different IDL.
func (m *codecMap) Protobuf() Codec {
	if pb, ok := m.codecs[codecNameProto]; ok {
		return pb
	}
	return &protoBinaryCodec{}
}

// Names returns a copy of the registered codec names. The returned slice is
// safe for the caller to mutate.
func (m *codecMap) Names() []string {
	names := make([]string, 0, len(m.codecs))
	for name := range m.codecs {
		names = append(names, name)
	}
	return names
}
