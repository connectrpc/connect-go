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
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/quick"

	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

func convertMapToInterface(stringMap map[string]string) map[string]interface{} {
	interfaceMap := make(map[string]interface{})
	for key, value := range stringMap {
		interfaceMap[key] = value
	}
	return interfaceMap
}

func TestCodecRoundTrips(t *testing.T) {
	t.Parallel()
	makeRoundtrip := func(codec Codec) func(string, int64) bool {
		return func(text string, number int64) bool {
			got := pingv1.PingRequest{}
			want := pingv1.PingRequest{Text: text, Number: number}
			data, err := codec.Marshal(&want)
			if err != nil {
				t.Fatal(err)
			}
			err = codec.Unmarshal(data, &got)
			if err != nil {
				t.Fatal(err)
			}
			return proto.Equal(&got, &want)
		}
	}
	if err := quick.Check(makeRoundtrip(&protoBinaryCodec{}), nil /* config */); err != nil {
		t.Error(err)
	}
	if err := quick.Check(makeRoundtrip(&protoJSONCodec{}), nil /* config */); err != nil {
		t.Error(err)
	}
}

func TestAppendCodec(t *testing.T) {
	t.Parallel()
	makeRoundtrip := func(codec marshalAppender) func(string, int64) bool {
		var data []byte
		return func(text string, number int64) bool {
			got := pingv1.PingRequest{}
			want := pingv1.PingRequest{Text: text, Number: number}
			data = data[:0]
			var err error
			data, err = codec.MarshalAppend(data, &want)
			if err != nil {
				t.Fatal(err)
			}
			err = codec.Unmarshal(data, &got)
			if err != nil {
				t.Fatal(err)
			}
			return proto.Equal(&got, &want)
		}
	}
	if err := quick.Check(makeRoundtrip(&protoBinaryCodec{}), nil /* config */); err != nil {
		t.Error(err)
	}
	if err := quick.Check(makeRoundtrip(&protoJSONCodec{}), nil /* config */); err != nil {
		t.Error(err)
	}
}

func TestMarshalAppendWhenOptionsSetEmitsUnpopulated(t *testing.T) {
	t.Parallel()

	req := pingv1.PingRequest{Text: "", Number: 1}
	expectedJSON := `{"number":"1","text":""}`
	codec := protoJSONCodec{emitDefaultValues: true}
	var data []byte
	data, err := codec.MarshalAppend(data, &req)
	if err != nil {
		t.Fatal(fmt.Errorf("doing MarshalAppend: %w", err))
	}
	// JSON produced by protojson is indeterministic by design: https://protobuf.dev/reference/go/faq/#unstable-json,
	// so we normalize the json here
	normalize := func(src string) string {
		var blob interface{}
		err := json.Unmarshal([]byte(src), &blob)
		if err != nil {
			t.Fatalf("Unmarshalling %s: %v", src, err)
		}
		normalized, err := json.Marshal(blob)
		if err != nil {
			t.Fatalf("Marshalling %s: %v", src, err)
		}
		return string(normalized)
	}
	if normalize(string(data)) != normalize(expectedJSON) {
		t.Errorf("expected: %s, got: %s", expectedJSON, string(data))
	}
}

func TestStableCodec(t *testing.T) {
	t.Parallel()
	makeRoundtrip := func(codec stableCodec) func(map[string]string) bool {
		return func(input map[string]string) bool {
			initialProto, err := structpb.NewStruct(convertMapToInterface(input))
			if err != nil {
				t.Fatal(err)
			}
			want, err := codec.MarshalStable(initialProto)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 10; i++ {
				roundtripProto := &structpb.Struct{}
				err = codec.Unmarshal(want, roundtripProto)
				if err != nil {
					t.Fatal(err)
				}
				got, err := codec.MarshalStable(roundtripProto)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					return false
				}
			}
			return true
		}
	}
	if err := quick.Check(makeRoundtrip(&protoBinaryCodec{}), nil /* config */); err != nil {
		t.Error(err)
	}
	if err := quick.Check(makeRoundtrip(&protoJSONCodec{}), nil /* config */); err != nil {
		t.Error(err)
	}
}

func TestJSONCodec(t *testing.T) {
	t.Parallel()

	codec := &protoJSONCodec{name: "json"}

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		err := codec.Unmarshal([]byte("{}"), &emptypb.Empty{})
		assert.Nil(t, err)
	})

	t.Run("unknown fields", func(t *testing.T) {
		t.Parallel()
		err := codec.Unmarshal([]byte(`{"foo": "bar"}`), &emptypb.Empty{})
		assert.Nil(t, err)
	})

	t.Run("empty string", func(t *testing.T) {
		t.Parallel()
		err := codec.Unmarshal([]byte{}, &emptypb.Empty{})
		assert.NotNil(t, err)
		assert.True(
			t,
			strings.Contains(err.Error(), "valid JSON"),
			assert.Sprintf(`error message should explain that "" is not a valid JSON object`),
		)
	})
}
