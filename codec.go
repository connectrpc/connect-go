package connect

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	codecNameProtobuf = "protobuf"
	codecNameJSON     = "json"
)

// A Codec can marshal structs (typically generated from a schema) to and from
// bytes.
type Codec interface {
	Name() string
	Marshal(any) ([]byte, error)
	Unmarshal([]byte, any) error
}

type codecProtobufBinary struct{}

var _ Codec = (*codecProtobufBinary)(nil)

func (c *codecProtobufBinary) Name() string { return codecNameProtobuf }

func (c *codecProtobufBinary) Marshal(message any) ([]byte, error) {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return nil, errNotProtobuf(message)
	}
	return proto.Marshal(protoMessage)
}

func (c *codecProtobufBinary) Unmarshal(bs []byte, message any) error {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return errNotProtobuf(message)
	}
	return proto.Unmarshal(bs, protoMessage)
}

type codecProtobufJSON struct {
	marshalOptions   protojson.MarshalOptions
	unmarshalOptions protojson.UnmarshalOptions
}

var _ Codec = (*codecProtobufJSON)(nil)

func (c *codecProtobufJSON) Name() string { return codecNameJSON }

func (c *codecProtobufJSON) Marshal(message any) ([]byte, error) {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return nil, errNotProtobuf(message)
	}
	return c.marshalOptions.Marshal(protoMessage)
}

func (c *codecProtobufJSON) Unmarshal(bs []byte, message any) error {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return errNotProtobuf(message)
	}
	return c.unmarshalOptions.Unmarshal(bs, protoMessage)
}

func errNotProtobuf(m any) error {
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
	if pb, ok := m.codecs[codecNameProtobuf]; ok {
		return pb
	}
	return &codecProtobufBinary{}
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
