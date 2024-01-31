// Copyright 2021-2024 The Connect Authors
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
	"testing"
)

func TestWithCustomProtoJSONWhenEmitDefaultValuesSetsConfig(t *testing.T) {
	t.Parallel()

	config := clientConfig{}
	option := WithCustomProtoJSON(ProtoJSONOptions{EmitDefaultValues: true, EmitUnpopulatedValues: true})
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
