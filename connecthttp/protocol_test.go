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
	"testing"

	"connectrpc.com/connect/v2/internal/assert"
)

func TestCanonicalizeContentType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		arg  string
		want string
	}{
		{name: "uppercase should be normalized", arg: "APPLICATION/json", want: "application/json"},
		{name: "utf-8 charset param should be stripped", arg: "application/json; charset=UTF-8", want: "application/json"},
		{name: "non-utf-8 charset param should be lowercased", arg: "application/json; charset=Shift-JIS", want: "application/json; charset=shift-jis"},
		{name: "non charset param should not be changed", arg: "multipart/form-data; boundary=fooBar", want: "multipart/form-data; boundary=fooBar"},
		{name: "no parameters should be normalized", arg: "APPLICATION/json;  ", want: "application/json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, canonicalizeContentType(tt.arg), tt.want)
		})
	}
}

func BenchmarkCanonicalizeContentType(b *testing.B) {
	b.Run("simple", func(b *testing.B) {
		for b.Loop() {
			_ = canonicalizeContentType("application/json")
		}
		b.ReportAllocs()
	})

	b.Run("with charset", func(b *testing.B) {
		for b.Loop() {
			_ = canonicalizeContentType("application/json; charset=utf-8")
		}
		b.ReportAllocs()
	})

	b.Run("with other param", func(b *testing.B) {
		for b.Loop() {
			_ = canonicalizeContentType("application/json; foo=utf-8")
		}
		b.ReportAllocs()
	})
}
