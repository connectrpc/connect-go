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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// TestMain runs the migration tool for testscript.
func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"migrate": func() { os.Exit(runMain(os.Args[1:])) },
	})
}

// TestScripts runs each testdata/script/*.txtar as a testscript: it
// materializes a module (shared go.mod/go.sum seeded by Setup, generated stubs
// chosen with the `stubs` command, plus the script's own source files), runs
// the tool, and asserts on its output and on go build. Golden output is
// kept in -- out.txt -- archive sections; UPDATE=1 rewrites them in place.
func TestScripts(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("materializes modules and shells out to go build; skipped under -short")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	scaffold, err := filepath.Abs(filepath.Join("testdata", "build"))
	if err != nil {
		t.Fatalf("scaffold dir: %v", err)
	}
	testscript.Run(t, testscript.Params{
		Dir:                 filepath.Join("testdata", "script"),
		RequireExplicitExec: true,
		UpdateScripts:       os.Getenv("UPDATE") != "",
		Cmds: map[string]func(*testscript.TestScript, bool, []string){
			"stubs": cmdStubs,
		},
		Setup: func(env *testscript.Env) error {
			// Seed the shared module files unless the case owns its module: a
			// case that ships its own go.mod (nested or self-contained module
			// fixtures) manages its own deps, so leave it untouched. go.mod's
			// replace target is resolved to this checkout so
			// connectrpc.com/connect/v2 builds locally.
			if !testArchiveHasGoMod(t, env.WorkDir) {
				gomod, err := os.ReadFile(filepath.Join(scaffold, "go.mod.txt"))
				if err != nil {
					return err
				}
				gomod = []byte(strings.ReplaceAll(string(gomod), "REPLACE_DIR", repoRoot))
				if err := os.WriteFile(filepath.Join(env.WorkDir, "go.mod"), gomod, 0o644); err != nil {
					return err
				}
				if err := copyFile(filepath.Join(scaffold, "go.sum"), filepath.Join(env.WorkDir, "go.sum")); err != nil {
					return err
				}
			}
			// The stubs command reads the scaffold to pick a generated tree, and
			// REPO lets archive-owned go.mod files resolve the local v2 module.
			env.Setenv("SCAFFOLD", scaffold)
			env.Setenv("REPO", repoRoot)
			// Keep diff output deterministic regardless of how the harness wires
			// stdout: the tool colorizes only without NO_COLOR and on a TTY.
			env.Setenv("NO_COLOR", "1")
			// testscript starts from a bare env; forward the Go toolchain's
			// cache and module settings so go build and go/packages resolve
			// offline from the host cache.
			for _, name := range []string{
				"GOMODCACHE", "GOCACHE", "GOPATH", "GOPROXY",
				"GOSUMDB", "GOFLAGS", "GOTOOLCHAIN", "GO111MODULE",
			} {
				if val, ok := os.LookupEnv(name); ok {
					env.Setenv(name, val)
				} else if val := goEnv(t.Context(), name); val != "" {
					env.Setenv(name, val)
				}
			}
			return nil
		},
	})
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

// cmdStubs copies the shared generated stubs for the requested connect version
// into the module's gen/ tree, choosing whether the tool sees v1 or v2 stubs:
//
//	stubs v2         connect v2 stubs (the tool rewrites against them)
//	stubs v1generic  connect v1 stubs, generic form (regenerate-first advice)
//	stubs v1simple   connect v1 stubs, simple form
func cmdStubs(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: stubs v2|v1generic|v1simple")
	}
	var tree string
	switch args[0] {
	case "v2":
		tree = "genv2"
	case "v1generic":
		tree = "genv1generic"
	case "v1simple":
		tree = "genv1simple"
	default:
		ts.Fatalf("unknown stubs %q: want v2, v1generic, or v1simple", args[0])
	}
	scaffold := ts.Getenv("SCAFFOLD")
	const pbRel = "connect/ping/v1/ping.pb.go"
	const connectRel = "connect/ping/v1/pingv1connect/ping.connect.go"
	ts.Check(copyFile(
		filepath.Join(scaffold, "gen", pbRel),
		ts.MkAbs(filepath.Join("gen", pbRel)),
	))
	ts.Check(copyFile(
		filepath.Join(scaffold, tree, connectRel),
		ts.MkAbs(filepath.Join("gen", connectRel)),
	))
}

// testArchiveHasGoMod reports whether the extracted archive already contains a
// go.mod anywhere under the work dir. When it does, the case owns its module
// and Setup must not overwrite it with the shared scaffold.
func testArchiveHasGoMod(t *testing.T, workDir string) bool {
	t.Helper()
	found := false
	_ = filepath.WalkDir(workDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if !entry.IsDir() && entry.Name() == "go.mod" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
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

// goEnv returns a single `go env` value, used as a fallback when a Go setting
// is not present in the process environment.
func goEnv(ctx context.Context, name string) string {
	out, err := exec.CommandContext(ctx, "go", "env", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func changedRewrite(string, []byte) ([]byte, Report, error) {
	return []byte("package p\n"), Report{Changed: true}, nil
}
