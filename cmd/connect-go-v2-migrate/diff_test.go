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

import (
	"strings"
	"testing"
)

// TestUnifiedDiffContext checks that a change is shown with surrounding context
// lines and a hunk header, while unchanged lines far from any change are elided.
func TestUnifiedDiffContext(t *testing.T) {
	t.Parallel()
	lines := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo"}
	old := strings.Join(lines, "\n")
	changed := append([]string(nil), lines...)
	changed[5] = "FOXTROT" // change the middle line
	out := unifiedDiff("file.go", []byte(old), []byte(strings.Join(changed, "\n")), false)

	for _, want := range []string{
		"@@ ",      // hunk header
		"-foxtrot", // removed line
		"+FOXTROT", // added line
		" delta",   // context before (3 lines)
		" echo",    //
		" golf",    // context after
		" india",   //
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff missing %q\n%s", want, out)
		}
	}
	// Lines beyond the context window are elided.
	for _, unwanted := range []string{"alpha", "bravo", "kilo"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("diff should have elided %q\n%s", unwanted, out)
		}
	}
}

// TestUnifiedDiffColor checks that color wraps removed/added/header lines in the
// ANSI codes only when requested.
func TestUnifiedDiffColor(t *testing.T) {
	t.Parallel()
	old := []byte("keep\nold\n")
	updated := []byte("keep\nnew\n")

	plain := unifiedDiff("file.go", old, updated, false)
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("color=false should emit no ANSI codes:\n%q", plain)
	}

	colored := unifiedDiff("file.go", old, updated, true)
	for _, want := range []string{colorRed, colorGreen, colorCyan, colorReset} {
		if !strings.Contains(colored, want) {
			t.Errorf("color=true missing escape %q:\n%q", want, colored)
		}
	}
}

// TestWantColor covers the NO_COLOR opt-out. The TTY branch depends on the
// environment (tests run with a non-terminal stdout), so both paths here
// resolve to no color; the assertion pins the NO_COLOR contract.
func TestWantColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if wantColor() {
		t.Error("wantColor() with NO_COLOR set = true, want false")
	}
}
