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
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// TestSkipWalkDir covers the by-name directory pruning: hidden ".dir" trees and
// the well-known dependency and fixture directories are skipped, real source
// directories are not.
func TestSkipWalkDir(t *testing.T) {
	t.Parallel()
	skip := []string{".git", ".github", ".idea", ".vscode", "vendor", "node_modules", "testdata"}
	keep := []string{"pkg", "internal", "cmd", "gen", "proto"}
	for _, name := range skip {
		if !skipWalkDir(name) {
			t.Errorf("skipWalkDir(%q) = false, want true", name)
		}
	}
	for _, name := range keep {
		if skipWalkDir(name) {
			t.Errorf("skipWalkDir(%q) = true, want false", name)
		}
	}
}

// TestWalkProject verifies that the walk visits files in real source
// directories and prunes hidden, dependency, fixture, and gitignored trees.
func TestWalkProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// .gitignore prunes a bare name (any depth) and an anchored path.
	writeFile(t, filepath.Join(root, ".gitignore"), "build\n/dist\n*.log\n")
	for _, rel := range []string{
		"buf.gen.yaml",                // visited
		"src/buf.gen.yaml",            // visited
		"src/build/buf.gen.yaml",      // pruned: gitignore "build" at any depth
		"dist/buf.gen.yaml",           // pruned: gitignore "/dist" anchored at root
		".github/workflows/ci.yaml",   // pruned: hidden
		"node_modules/p/buf.gen.yaml", // pruned: dependency tree
		"testdata/buf.gen.yaml",       // pruned: fixture tree
	} {
		writeFile(t, filepath.Join(root, rel), "x\n")
	}

	var visited []string
	if err := walkProject(root, func(path string) error {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		visited = append(visited, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(visited)

	want := []string{".gitignore", "buf.gen.yaml", "src/buf.gen.yaml"}
	if !slices.Equal(visited, want) {
		t.Errorf("walkProject visited %v, want %v", visited, want)
	}
}

// TestIsIgnoredDir covers anchored vs floating gitignore directory patterns and
// scoping to the directory the .gitignore lived in.
func TestIsIgnoredDir(t *testing.T) {
	t.Parallel()
	base := filepath.FromSlash("/repo")
	rules := []ignoreRule{
		{base: base, pattern: "build", anchored: false}, // any depth
		{base: base, pattern: "dist", anchored: true},   // only /repo/dist
	}
	tests := []struct {
		dir  string
		want bool
	}{
		{dir: "/repo/build", want: true},
		{dir: "/repo/src/build", want: true}, // floating matches at depth
		{dir: "/repo/dist", want: true},      // anchored matches at root
		{dir: "/repo/src/dist", want: false}, // anchored does not match at depth
		{dir: "/repo/src", want: false},
		{dir: "/other/build", want: false}, // outside the rule's scope
	}
	for _, test := range tests {
		if got := isIgnoredDir(filepath.FromSlash(test.dir), rules); got != test.want {
			t.Errorf("isIgnoredDir(%q) = %v, want %v", test.dir, got, test.want)
		}
	}
}
