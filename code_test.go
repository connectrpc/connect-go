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
	"strings"
	"testing"

	"github.com/bufbuild/connect-go/internal/assert"
)

func TestCode(t *testing.T) {
	t.Parallel()
	var valid []Code
	for code := minCode; code <= maxCode; code++ {
		valid = append(valid, code)
	}
	invalid := Code(999)
	assert.True(t, invalid > maxCode)
	assert.True(t, strings.HasPrefix(invalid.String(), "Code("))
	assert.True(t, strings.HasPrefix(invalid.AllCapsString(), "CODE_"))
	// Ensures that we don't forget to update the mapping in these methods.
	for _, code := range valid {
		assert.False(
			t,
			strings.HasPrefix(code.String(), "Code("),
			assert.Sprintf("update Code.String() for new code %v", code),
		)
		assert.False(
			t,
			strings.HasPrefix(code.AllCapsString(), "CODE_"),
			assert.Sprintf("update Code.AllCapsString() for new code %v", code),
		)
	}
}
