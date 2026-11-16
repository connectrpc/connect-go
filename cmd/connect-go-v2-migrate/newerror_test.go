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

// TestScanWrapVerbs covers counting the %w verbs in a fmt.Errorf format and
// finding the argument the first one wraps: the index is known only while the
// verbs map to arguments in order.
func TestScanWrapVerbs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		format string
		count  int
		index  int
	}{
		{format: "wrap: %w", count: 1, index: 0},
		{format: "%s %d: %w", count: 1, index: 2},
		{format: "%w occurred for %s", count: 1, index: 0},
		{format: "100%% %-8.3f %+v: %w", count: 1, index: 2},
		{format: "wrap: %+w", count: 1, index: 0},
		{format: "no wrap: %v", count: 0, index: -1},
		{format: "100%%w", count: 0, index: -1},
		{format: "%w then %w", count: 2, index: 0},
		{format: "%[2]s: %[1]w", count: 1, index: -1},
		{format: "%*d: %w", count: 1, index: -1},
		{format: "%w: %*d", count: 1, index: 0},
		{format: "trailing %", count: 0, index: -1},
	} {
		count, index := scanWrapVerbs(tt.format)
		if count != tt.count || index != tt.index {
			t.Errorf("scanWrapVerbs(%q) = %d, %d; want %d, %d", tt.format, count, index, tt.count, tt.index)
		}
	}
}
