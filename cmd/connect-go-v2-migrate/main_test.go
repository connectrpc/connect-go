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
	"os"
	"path/filepath"
	"testing"
)

// migrateExecEnv marks a re-exec of the test binary as a `migrate` run:
// script `exec migrate` commands re-run this binary with it set, and
// TestMain dispatches to the real tool instead of the tests.
const migrateExecEnv = "MIGRATE_TEST_EXEC"

func TestMain(m *testing.M) {
	if os.Getenv(migrateExecEnv) == "1" {
		os.Exit(runMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func TestProcessFileWriteFailure(t *testing.T) {
	t.Parallel()
	// A path under a directory that does not exist makes os.WriteFile fail.
	badPath := filepath.Join(t.TempDir(), "missing", "x.go")
	var run results
	processFile(fileContent{path: badPath, content: []byte("package q\n")}, changedRewrite, true, &run)
	if len(run.rewrites) != 0 {
		t.Errorf("a failed write must not be recorded as a rewrite; got %d", len(run.rewrites))
	}
	if run.errored != 1 {
		t.Errorf("a failed write must increment errored; got %d", run.errored)
	}
}

func TestProcessFileDryRunRecords(t *testing.T) {
	t.Parallel()
	var run results
	processFile(fileContent{path: "x.go", content: []byte("package q\n")}, changedRewrite, false, &run)
	if len(run.rewrites) != 1 {
		t.Errorf("dry-run should record the proposed rewrite; got %d", len(run.rewrites))
	}
	if run.errored != 0 {
		t.Errorf("dry-run should not error; got %d", run.errored)
	}
}

func TestWriteFilePreservingMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []os.FileMode{0o600, 0o644, 0o664, 0o755} {
		path := filepath.Join(t.TempDir(), "src.go")
		if err := os.WriteFile(path, []byte("before"), mode); err != nil {
			t.Fatal(err)
		}
		// Chmod explicitly: the umask would otherwise clear bits at creation.
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := writeFilePreservingMode(path, []byte("after")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != mode {
			t.Errorf("mode = %v, want %v", got, mode)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "after" {
			t.Errorf("content = %q, want %q", content, "after")
		}
	}

	fresh := filepath.Join(t.TempDir(), "new.go")
	if err := writeFilePreservingMode(fresh, []byte("x")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	// The umask may clear bits on creation, so assert only that nothing beyond
	// 0644 was granted.
	if got := info.Mode().Perm(); got&^0o644 != 0 {
		t.Errorf("new file mode = %v, want no bits beyond %v", got, os.FileMode(0o644))
	}
}

func TestVersionFlag(t *testing.T) {
	t.Parallel()
	if got := runMain([]string{"-version"}); got != 0 {
		t.Errorf("runMain(-version) = %d, want 0", got)
	}
	if toolVersion() == "" {
		t.Error("toolVersion() is empty")
	}
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func changedRewrite(string, []byte) ([]byte, Report, error) {
	return []byte("package p\n"), Report{Changed: true}, nil
}
