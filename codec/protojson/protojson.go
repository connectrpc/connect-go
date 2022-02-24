package protojson

import (
	"fmt"

	"github.com/bufbuild/connect/codec"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Name is the name connect uses for JavaScript Object Notation.
const Name = "json"

// Codec marshals protobuf to and from JSON using the mapping defined by the
// protocol buffer specification.
type Codec struct {
	marshalOptions   protojson.MarshalOptions
	unmarshalOptions protojson.UnmarshalOptions
}

// New constructs a JSON protobuf codec.
//
// It uses the default mapping specified by the protocol buffer specification:
// fields are named using lowerCamelCase, zero values are omitted, missing
// required fields are errors, enums are emitted as strings, etc.
func New() *Codec {
	return &Codec{}
}

var _ codec.Codec = (*Codec)(nil)

// Marshal implements codec.Codec.
func (c *Codec) Marshal(message any) ([]byte, error) {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return nil, errNotProtobuf(message)
	}
	return c.marshalOptions.Marshal(protoMessage)
}

// Unmarshal implements codec.Codec.
func (c *Codec) Unmarshal(bs []byte, message any) error {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return errNotProtobuf(message)
	}
	return c.unmarshalOptions.Unmarshal(bs, protoMessage)
}

func errNotProtobuf(m any) error {
	return fmt.Errorf("%T doesn't implement proto.Message", m)
}
