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

package connecterr

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
)

func TestAsErrorRejectsTypedNil(t *testing.T) {
	t.Parallel()
	var typedNil *connect.Error
	for name, err := range map[string]error{
		"bare":    typedNil,
		"wrapped": fmt.Errorf("handler: %w", typedNil),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			connectErr, ok := AsError(err)
			assert.False(t, ok)
			assert.Nil(t, connectErr)
		})
	}
}

func TestScrubHandlerErrorTypedNil(t *testing.T) {
	t.Parallel()
	scrubbed := ScrubHandlerError((*connect.Error)(nil))
	connectErr, ok := errors.AsType[*connect.Error](scrubbed)
	assert.True(t, ok)
	assert.NotNil(t, connectErr)
	assert.Equal(t, connectErr.Code(), connect.CodeUnknown)
	assert.Equal(t, connectErr.Message(), "")
}
