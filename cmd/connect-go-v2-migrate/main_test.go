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

// TestMain lets the test binary stand in for the migrate binary.
func TestMain(m *testing.M) {
	if os.Getenv(migrateExecEnv) == "1" {
		os.Exit(runMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// TestProcessFileWriteFailure checks that a file whose write fails is not
// recorded as an applied rewrite: a failed write counts as an error, never a
// success, so the report cannot claim a change that never reached disk.
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

// TestProcessFileDryRunRecords checks the dry-run path still records the
// proposed rewrite: nothing is written, so there is no failure to gate on.
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
