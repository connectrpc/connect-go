package connect

import (
	"testing"
)

func TestWithCustomProtoJSONWhenEmitDefaultValuesSetsConfig(t *testing.T) {
	config := clientConfig{}
	option := WithCustomProtoJSON(ProtoJsonOptions{EmitDefaultValues: true, EmitUnpopulatedValues: true})
	option.applyToClient(&config)
	codec, ok := config.Codec.(*protoJSONCodec)
	if !ok {
		t.Errorf("casting to protoJSONCodec failed, want protoJSONCodec, got: %T", config.Codec)
	}
	if codec.emitDefaultValues != true {
		t.Errorf("emitDefaultValues = %v, want %v", codec.emitDefaultValues, true)
	}
	if codec.emitUnpopulatedValues != true {
		t.Errorf("emitUnpopulatedValues = %v, want %v", codec.emitUnpopulatedValues, true)
	}
}
