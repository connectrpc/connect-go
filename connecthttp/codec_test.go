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
	"bytes"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

func convertMapToInterface(stringMap map[string]string) map[string]any {
	interfaceMap := make(map[string]any)
	for key, value := range stringMap {
		interfaceMap[key] = value
	}
	return interfaceMap
}

func TestCodecRoundTrips(t *testing.T) {
	t.Parallel()
	makeRoundtrip := func(codec connect.Codec) func(string, int64) bool {
		return func(text string, number int64) bool {
			want := pingv1.PingRequest{Text: text, Number: number}
			var buffer bytes.Buffer
			if err := codec.MarshalWrite(t.Context(), &buffer, &want); err != nil {
				t.Fatal(err)
			}
			got := pingv1.PingRequest{}
			if err := codec.UnmarshalRead(t.Context(), &buffer, &got); err != nil {
				t.Fatal(err)
			}
			return proto.Equal(&got, &want)
		}
	}
	if err := quick.Check(makeRoundtrip(&connectproto.BinaryCodec{}), nil /* config */); err != nil {
		t.Error(err)
	}
	if err := quick.Check(makeRoundtrip(&connectproto.JSONCodec{}), nil /* config */); err != nil {
		t.Error(err)
	}
}

func TestStableCodec(t *testing.T) {
	t.Parallel()
	makeRoundtrip := func(codec connect.StableCodec) func(map[string]string) bool {
		return func(input map[string]string) bool {
			initialProto, err := structpb.NewStruct(convertMapToInterface(input))
			if err != nil {
				t.Fatal(err)
			}
			var buffer bytes.Buffer
			if err := codec.MarshalWriteStable(t.Context(), &buffer, initialProto); err != nil {
				t.Fatal(err)
			}
			want := buffer.Bytes()
			for range 10 {
				roundtripProto := &structpb.Struct{}
				if err := codec.UnmarshalRead(t.Context(), bytes.NewReader(want), roundtripProto); err != nil {
					t.Fatal(err)
				}
				var got bytes.Buffer
				if err := codec.MarshalWriteStable(t.Context(), &got, roundtripProto); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got.Bytes(), want) {
					return false
				}
			}
			return true
		}
	}
	if err := quick.Check(makeRoundtrip(&connectproto.BinaryCodec{}), nil /* config */); err != nil {
		t.Error(err)
	}
	if err := quick.Check(makeRoundtrip(&connectproto.JSONCodec{}), nil /* config */); err != nil {
		t.Error(err)
	}
}

func TestJSONCodec(t *testing.T) {
	t.Parallel()

	codec := connectproto.NewJSONCodec()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		err := codec.UnmarshalRead(t.Context(), strings.NewReader("{}"), &emptypb.Empty{})
		assert.Nil(t, err)
	})

	t.Run("unknown fields", func(t *testing.T) {
		t.Parallel()
		err := codec.UnmarshalRead(t.Context(), strings.NewReader(`{"foo": "bar"}`), &emptypb.Empty{})
		assert.Nil(t, err)
	})

	t.Run("empty string", func(t *testing.T) {
		t.Parallel()
		err := codec.UnmarshalRead(t.Context(), strings.NewReader(""), &emptypb.Empty{})
		assert.NotNil(t, err)
		assert.True(
			t,
			strings.Contains(err.Error(), "valid JSON"),
			assert.Sprintf(`error message should explain that "" is not a valid JSON object`),
		)
	})
}

func TestCodecsOption(t *testing.T) {
	t.Parallel()

	names := func(options ...Option) []string {
		opts := defaultOptions()
		for _, option := range options {
			option.apply(&opts)
		}
		got := newReadOnlyCodecs(opts.codecs).Names()
		slices.Sort(got)
		return got
	}

	t.Run("defaults to proto and json", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, names(), []string{connect.CodecNameJSON, connect.CodecNameProto})
	})
	t.Run("replaces the defaults", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, names(WithCodecs(connectproto.NewBinaryCodec())), []string{connect.CodecNameProto})
	})
	t.Run("last call wins", func(t *testing.T) {
		t.Parallel()
		opts := []Option{WithCodecs(connectproto.NewJSONCodec()), WithCodecs(connectproto.NewBinaryCodec())}
		assert.Equal(t, names(opts...), []string{connect.CodecNameProto})
	})
	t.Run("sends with the first codec by default", func(t *testing.T) {
		t.Parallel()
		sendCodec := func(options ...Option) string {
			opts := defaultOptions()
			for _, option := range options {
				option.apply(&opts)
			}
			return opts.getSendCodecName()
		}
		assert.Equal(t, sendCodec(), connect.CodecNameProto)
		assert.Equal(t, sendCodec(
			WithCodecs(connectproto.NewJSONCodec(), connectproto.NewBinaryCodec()),
		), connect.CodecNameJSON)
		// WithSendCodec overrides the first-listed default.
		assert.Equal(t, sendCodec(
			WithCodecs(connectproto.NewJSONCodec(), connectproto.NewBinaryCodec()),
			WithSendCodec(connect.CodecNameProto),
		), connect.CodecNameProto)
		assert.Equal(t, sendCodec(WithCodecs()), "")
	})
	t.Run("repeated name is dropped", func(t *testing.T) {
		t.Parallel()
		opts := WithCodecs(connectproto.NewBinaryCodec(), connectproto.NewJSONCodec(), connectproto.NewBinaryCodec())
		assert.Equal(t, names(opts), []string{connect.CodecNameJSON, connect.CodecNameProto})
	})
}
