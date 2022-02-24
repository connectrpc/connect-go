// Package protobuf provides default codec.Codec implementations for types
// generated from protocol buffer schemas.
package protobuf

import (
	"fmt"

	"github.com/bufbuild/connect/codec"
	"google.golang.org/protobuf/proto"
)

// Name is the name connect uses for the protocol buffer language's binary
// encoding.
const Name = "protobuf"

// Codec marshals protobuf structs to and from binary.
type Codec struct{}

var _ codec.Codec = (*Codec)(nil)

// NewBinary constructs a Binary protobuf codec.
func New() *Codec {
	return &Codec{}
}

// Marshal implements codec.Codec.
func (c *Codec) Marshal(message any) ([]byte, error) {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return nil, errNotProtobuf(message)
	}
	return proto.Marshal(protoMessage)
}

// Unmarshal implements codec.Codec.
func (c *Codec) Unmarshal(bs []byte, message any) error {
	protoMessage, ok := message.(proto.Message)
	if !ok {
		return errNotProtobuf(message)
	}
	return proto.Unmarshal(bs, protoMessage)
}

func errNotProtobuf(m any) error {
	return fmt.Errorf("%T doesn't implement proto.Message", m)
}
