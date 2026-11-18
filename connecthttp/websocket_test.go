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
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
)

// Options appended for one spec must not land in another's, which they would
// if two clones shared a backing array with room to grow.
func TestForSpecClonesWebSocketOptions(t *testing.T) {
	t.Parallel()
	opts := defaultOptions()
	WithWebSocketKeepAlive(time.Second).apply(&opts)
	opts.websocketServerOptions = slices.Grow(opts.websocketServerOptions, 4)
	opts.websocketClientOptions = slices.Grow(opts.websocketClientOptions, 4)
	WithConditionalOptions(func(connect.Spec) []Option {
		return []Option{WithWebSocketKeepAliveTimeout(time.Minute)}
	}).apply(&opts)

	first := opts.forSpec(connect.Spec{Procedure: "/a.A/One"})
	second := opts.forSpec(connect.Spec{Procedure: "/a.A/Two"})
	assert.Equal(t, len(opts.websocketServerOptions), 1)
	assert.Equal(t, len(first.websocketServerOptions), 2)
	assert.True(t, &first.websocketServerOptions[1] != &second.websocketServerOptions[1])
	assert.True(t, &first.websocketClientOptions[1] != &second.websocketClientOptions[1])
}
