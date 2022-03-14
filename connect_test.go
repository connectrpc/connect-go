// Copyright 2021-2022 Buf Technologies, Inc.
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
	"fmt"
	"testing"

	"github.com/bufbuild/connect/internal/assert"
)

func TestTransformRequest(t *testing.T) {
	n := 42
	msg := NewRequest(&n)
	msg.Header().Set("Foo", "bar")
	got := TransformRequest(msg, intPointerToStringPointer)
	assert.Equal(t, *got.Msg, "42")
	assert.Equal(t, got.Header().Get("Foo"), "bar")
	assert.Equal(t, got.Spec(), msg.Spec())
}

func TestTransformResponse(t *testing.T) {
	n := 42
	msg := NewResponse(&n)
	msg.Header().Set("Foo", "bar")
	msg.Trailer().Set("Baz", "quux")
	got := TransformResponse(msg, intPointerToStringPointer)
	assert.Equal(t, *got.Msg, "42")
	assert.Equal(t, got.Header().Get("Foo"), "bar")
	assert.Equal(t, got.Trailer().Get("Baz"), "quux")
}

func intPointerToStringPointer(n *int) *string {
	if n == nil {
		return nil
	}
	s := fmt.Sprint(*n)
	return &s
}
