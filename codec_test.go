// Copyright 2021-2023 Buf Technologies, Inc.
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

	"github.com/bufbuild/connect-go/internal/assert"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
)

func TestCodec(t *testing.T) {
	t.Parallel()
	assertCodecRoundTrips(t, &protoBinaryCodec{})
	assertCodecRoundTrips(t, &protoJSONCodec{})
	assertStableMarshalEquals(
		t,
		&protoBinaryCodec{},
		&pingv1.PingRequest{Text: "text", Number: 1}, []byte{
			// Comments are in the format of protoscope.
			// 1:VARINT 1
			0b00001_000, 1,
			// 2:LEN 4 "test"
			0b00010_010, 4, 0x74, 0x65, 0x78, 0x74,
		},
	)
	assertStableMarshalEquals(
		t,
		&protoJSONCodec{},
		&pingv1.PingRequest{Text: "text", Number: 1},
		[]byte(`{"number":"1","text":"text"}`),
	)
}

func assertCodecRoundTrips(tb testing.TB, codec Codec) {
	tb.Helper()
	got := pingv1.PingRequest{}
	want := pingv1.PingRequest{Text: "text", Number: 1}
	data, err := codec.Marshal(&want)
	assert.Nil(tb, err)
	assert.Nil(tb, codec.Unmarshal(data, &got))
	assert.Equal(tb, &got, &want)
}

func assertStableMarshalEquals(tb testing.TB, codec stableCodec, message any, want []byte) {
	tb.Helper()
	got, err := codec.MarshalStable(message)
	assert.Nil(tb, err)
	assert.Equal(tb, got, want)
}
