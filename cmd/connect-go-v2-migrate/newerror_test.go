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

package main

import "testing"

// TestWrapVerbIndex covers finding the argument a fmt.Errorf %w wraps: only a
// single %w whose verbs map to arguments in order is found.
func TestWrapVerbIndex(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		format string
		index  int
		ok     bool
	}{
		{format: "wrap: %w", index: 0, ok: true},
		{format: "%s %d: %w", index: 2, ok: true},
		{format: "%w occurred for %s", index: 0, ok: true},
		{format: "100%% %-8.3f %+v: %w", index: 2, ok: true},
		{format: "no wrap: %v", ok: false},
		{format: "%w then %w", ok: false},
		{format: "%[2]s: %[1]w", ok: false},
		{format: "%*d: %w", ok: false},
		{format: "trailing %", ok: false},
	} {
		index, ok := wrapVerbIndex(tt.format)
		if ok != tt.ok || (ok && index != tt.index) {
			t.Errorf("wrapVerbIndex(%q) = %d, %v; want %d, %v", tt.format, index, ok, tt.index, tt.ok)
		}
	}
}
