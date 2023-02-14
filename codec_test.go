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
	"strings"
	"testing"

	"connectrpc.com/connect/internal/assert"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestJSONCodec(t *testing.T) {
	t.Parallel()

	var empty emptypb.Empty
	codec := &protoJSONCodec{name: "json"}
	err := codec.Unmarshal([]byte{}, &empty)
	assert.NotNil(t, err)
	assert.True(
		t,
		strings.Contains(err.Error(), "valid JSON"),
		assert.Sprintf(`error message should explain that "" is not a valid JSON object`),
	)
}
