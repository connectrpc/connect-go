// Package protobuf provides default codec.Codec implementations for types
// generated from protocol buffer schemas.
package protobuf

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/bufconnect/connect/codec"
)

const (
	// NameBinary is the name connect uses for the protocol buffer language's
	// binary encoding.
	NameBinary = "protobuf"
	// NameJSON is the name connect uses for JavaScript Object Notation.
	NameJSON = "json"
)

// Binary marshals protobuf structs to and from binary.
type Binary struct{}

var _ codec.Codec = (*Binary)(nil)

// NewBinary constructs a Binary protobuf codec.
func NewBinary() *Binary {
	return &Binary{}
}

// Marshal implements codec.Codec.
func (b *Binary) Marshal(m any) ([]byte, error) {
	pm, ok := m.(proto.Message)
	if !ok {
		return nil, errNotProtobuf(m)
	}
	return proto.Marshal(pm)
}

// Unmarshal implements codec.Codec.
func (b *Binary) Unmarshal(bs []byte, m any) error {
	pm, ok := m.(proto.Message)
	if !ok {
		return errNotProtobuf(m)
	}
	return proto.Unmarshal(bs, pm)
}

// JSON marshals protobuf to and from JSON using the mapping defined by the
// protocol buffer specification.
type JSON struct {
	marshalOptions   protojson.MarshalOptions
	unmarshalOptions protojson.UnmarshalOptions
}

// NewJSON constructs a JSON protobuf codec.
//
// It uses the default mapping specified by the protocol buffer specification:
// fields are named using lowerCamelCase, zero values are omitted, missing
// required fields are errors, enums are emitted as strings, etc.
func NewJSON() *JSON {
	return &JSON{}
}

var _ codec.Codec = (*JSON)(nil)

// Marshal implements codec.Codec.
func (j *JSON) Marshal(m any) ([]byte, error) {
	pm, ok := m.(proto.Message)
	if !ok {
		return nil, errNotProtobuf(m)
	}
	return j.marshalOptions.Marshal(pm)
}

// Unmarshal implements codec.Codec.
func (j *JSON) Unmarshal(bs []byte, m any) error {
	pm, ok := m.(proto.Message)
	if !ok {
		return errNotProtobuf(m)
	}
	return j.unmarshalOptions.Unmarshal(bs, pm)
}

func errNotProtobuf(m any) error {
	return fmt.Errorf("%T doesn't implement proto.Message", m)
}
