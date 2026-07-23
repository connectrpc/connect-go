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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// walkProject calls visit for every file that survives pruning of hidden
// trees, vendor/node_modules/testdata, and .gitignore'd directories. Per-entry
// read errors are skipped; an error reading root itself is returned.
func walkProject(root string, visit func(path string) error) error {
	var rules []ignoreRule
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // skip unreadable sub-entries
		}
		if !entry.IsDir() {
			return visit(path)
		}
		if path != root && skipWalkDir(entry.Name()) {
			return filepath.SkipDir
		}
		abs, absErr := filepath.Abs(path)
		if absErr != nil {
			return nil //nolint:nilerr // can't resolve the path; walk past it rather than abort
		}
		if isIgnoredDir(abs, rules) {
			return filepath.SkipDir
		}
		// WalkDir is pre-order, so a directory's .gitignore rules are in place
		// before its children are visited.
		rules = appendGitignore(rules, abs)
		return nil
	})
}

// skipWalkDir reports whether a directory should be pruned by name: hidden
// directories and the well-known dependency/fixture trees.
func skipWalkDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "vendor", "node_modules", "testdata":
		return true
	}
	return false
}

// ignoreRule is one directory-pruning pattern from a .gitignore, scoped to the
// directory the file lived in.
type ignoreRule struct {
	base     string // absolute directory the .gitignore was read from
	pattern  string // cleaned pattern, slash-separated, no leading/trailing slash
	anchored bool   // pattern is relative to base (had a leading or internal slash)
}

// appendGitignore reads dir/.gitignore and appends its directory-pruning rules.
// Comments, blanks, negations, and globs are skipped: the walk only needs to
// avoid descending into ignored trees, never to match files.
func appendGitignore(rules []ignoreRule, dir string) []ignoreRule {
	content, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return rules
	}
	for line := range strings.SplitSeq(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		pattern := strings.TrimSuffix(line, "/")
		if strings.ContainsAny(pattern, "*?[") {
			continue // globs match files more often than dirs, so skip for safety
		}
		// A leading or internal slash anchors the pattern to base. A bare name
		// (optionally with a trailing slash) matches at any depth.
		anchored := strings.Contains(pattern, "/")
		pattern = strings.Trim(pattern, "/")
		if pattern == "" {
			continue
		}
		rules = append(rules, ignoreRule{base: dir, pattern: filepath.ToSlash(pattern), anchored: anchored})
	}
	return rules
}

// isIgnoredDir reports whether dir matches any gitignore rule in scope.
func isIgnoredDir(dir string, rules []ignoreRule) bool {
	for _, rule := range rules {
		rel, err := filepath.Rel(rule.base, dir)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || strings.HasPrefix(rel, "../") {
			continue // dir is not within the rule's scope
		}
		if rule.anchored {
			if rel == rule.pattern {
				return true
			}
			continue
		}
		if lastSegment(rel) == rule.pattern {
			return true
		}
	}
	return false
}

func lastSegment(path string) string {
	if index := strings.LastIndex(path, "/"); index >= 0 {
		return path[index+1:]
	}
	return path
}
