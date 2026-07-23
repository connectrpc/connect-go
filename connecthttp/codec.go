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

package connecthttp

import (
	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
)

const (
	codecNameJSONCharsetUTF8 = connect.CodecNameJSON + "; charset=utf-8"
)

// readOnlyCodecs is a read-only interface to a map of named codecs.
type readOnlyCodecs interface {
	// Get gets the Codec with the given name.
	Get(string) connect.Codec
	// Protobuf gets the user-supplied protobuf codec, falling back to the default
	// implementation if necessary.
	//
	// This is helpful in the gRPC protocol, where the wire protocol requires
	// marshaling protobuf structs to binary even if the RPC procedures were
	// generated from a different IDL.
	Protobuf() connect.Codec
	// Names returns a copy of the registered codec names. The returned slice is
	// safe for the caller to mutate.
	Names() []string
}

func newReadOnlyCodecs(nameToCodec map[string]connect.Codec) readOnlyCodecs {
	return &codecMap{
		nameToCodec: nameToCodec,
	}
}

type codecMap struct {
	nameToCodec map[string]connect.Codec
}

func (m *codecMap) Get(name string) connect.Codec {
	return m.nameToCodec[name]
}

func (m *codecMap) Protobuf() connect.Codec {
	if pb, ok := m.nameToCodec[connect.CodecNameProto]; ok {
		return pb
	}
	return &connectproto.BinaryCodec{}
}

func (m *codecMap) Names() []string {
	names := make([]string, 0, len(m.nameToCodec))
	for name := range m.nameToCodec {
		names = append(names, name)
	}
	return names
}
