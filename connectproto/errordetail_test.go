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

package connectproto

import (
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestErrorDetailRoundTrip(t *testing.T) {
	t.Parallel()
	msg := durationpb.New(time.Second)
	detail, err := NewErrorDetail(msg)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := detail.Type, "google.protobuf.Duration"; got != want {
		t.Errorf("Type = %q, want %q", got, want)
	}
	unmarshaled, err := UnmarshalErrorDetail(detail)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(unmarshaled, msg) {
		t.Errorf("round trip = %v, want %v", unmarshaled, msg)
	}
}

func TestErrorDetailAnyAsIs(t *testing.T) {
	t.Parallel()
	anyMsg, err := anypb.New(wrapperspb.String("hello"))
	if err != nil {
		t.Fatal(err)
	}
	detail, err := NewErrorDetail(anyMsg)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := detail.Type, "google.protobuf.StringValue"; got != want {
		t.Errorf("Type = %q, want %q", got, want)
	}
	if !proto.Equal(ErrorDetailToAny(detail), anyMsg) {
		t.Errorf("ErrorDetailToAny = %v, want %v", ErrorDetailToAny(detail), anyMsg)
	}
}

func TestErrorDetailPackFailure(t *testing.T) {
	t.Parallel()
	if _, err := NewErrorDetail(wrapperspb.String("\xc3\x28")); err == nil {
		t.Error("NewErrorDetail with invalid UTF-8 should fail")
	}
}

func TestErrorDetailToAnyPrefix(t *testing.T) {
	t.Parallel()
	bare := &connect.ErrorDetail{Type: "google.protobuf.StringValue"}
	if got, want := ErrorDetailToAny(bare).GetTypeUrl(), anyResolverPrefix+"google.protobuf.StringValue"; got != want {
		t.Errorf("TypeUrl = %q, want %q", got, want)
	}
	url := &connect.ErrorDetail{Type: "example.com/acme.v1.Custom"}
	if got, want := ErrorDetailToAny(url).GetTypeUrl(), "example.com/acme.v1.Custom"; got != want {
		t.Errorf("TypeUrl = %q, want %q", got, want)
	}
}

func TestTypeNameForURL(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		url      string
		typeName string
	}{
		{
			name:     "no-prefix",
			url:      "foo.bar.Baz",
			typeName: "foo.bar.Baz",
		},
		{
			name:     "standard-prefix",
			url:      anyResolverPrefix + "foo.bar.Baz",
			typeName: "foo.bar.Baz",
		},
		{
			name:     "different-hostname",
			url:      "abc.com/foo.bar.Baz",
			typeName: "foo.bar.Baz",
		},
		{
			name:     "additional-path-elements",
			url:      anyResolverPrefix + "abc/def/foo.bar.Baz",
			typeName: "foo.bar.Baz",
		},
		{
			name:     "full-url",
			url:      "https://abc.com/abc/def/foo.bar.Baz",
			typeName: "foo.bar.Baz",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := typeNameForURL(testCase.url); got != testCase.typeName {
				t.Errorf("typeNameForURL(%q) = %q, want %q", testCase.url, got, testCase.typeName)
			}
		})
	}
}

func TestErrorDetailUnknownType(t *testing.T) {
	t.Parallel()
	detail := &connect.ErrorDetail{Type: "acme.user.v1.User", Value: []byte{0xde, 0xad}}
	if _, err := UnmarshalErrorDetail(detail); err == nil {
		t.Error("UnmarshalErrorDetail with unregistered type should fail")
	}
}
