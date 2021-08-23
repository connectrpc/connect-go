package rerpc

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Codec defines the interface reRPC uses to encode and decode messages.
type Codec interface {
	Marshal(interface{}) ([]byte, error)
	Unmarshal([]byte, interface{}) error
}

// CodecProvider provides Codecs based on the Content-Type header.
type CodecProvider struct {
	jsonProtobuf jsonProtobufCodec
	protobuf     protobufCodec
}

// CodecProviderOption configures a CodecProvider.
// There are no current CodecProviderOptions implemented.
type CodecProviderOption interface {
	unimplemented()
}

// NewCodecProvider returns a new CodecProvider.
func NewCodecProvider(opts ...CodecProviderOption) *CodecProvider {
	return &CodecProvider{
		jsonProtobuf: jsonProtobufCodec{
			marshaler:   protojson.MarshalOptions{UseProtoNames: true},
			unmarshaler: protojson.UnmarshalOptions{DiscardUnknown: true},
		},
		protobuf: protobufCodec{},
	}
}

// CodecForContentType returns the Codec associated with the given Content-Type
// header value.
func (c *CodecProvider) CodecForContentType(contentType string) (Codec, bool) {
	switch contentType {
	case TypeJSON:
		return c.jsonProtobuf, true
	case TypeDefaultGRPC, TypeProtoGRPC, TypeProtoTwirp:
		return c.protobuf, true
	default:
		return nil, false
	}
}

type jsonProtobufCodec struct {
	marshaler   protojson.MarshalOptions
	unmarshaler protojson.UnmarshalOptions
}

func (c jsonProtobufCodec) Marshal(value interface{}) ([]byte, error) {
	protoMessage, ok := value.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("could not case %T to a proto.Message", value)
	}
	return c.marshaler.Marshal(protoMessage)
}

func (c jsonProtobufCodec) Unmarshal(data []byte, value interface{}) error {
	protoMessage, ok := value.(proto.Message)
	if !ok {
		return fmt.Errorf("could not case %T to a proto.Message", value)
	}
	return c.unmarshaler.Unmarshal(data, protoMessage)
}

type protobufCodec struct{}

func (protobufCodec) Marshal(value interface{}) ([]byte, error) {
	protoMessage, ok := value.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("could not case %T to a proto.Message", value)
	}
	return proto.Marshal(protoMessage)
}

func (protobufCodec) Unmarshal(data []byte, value interface{}) error {
	protoMessage, ok := value.(proto.Message)
	if !ok {
		return fmt.Errorf("could not case %T to a proto.Message", value)
	}
	return proto.Unmarshal(data, protoMessage)
}
