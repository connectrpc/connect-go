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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/tools/txtar"
)

// TestScripts runs the txtar archives in testdata/script, one per test,
// following the script-test pattern the Go project uses for cmd/go. An
// archive's comment is the script and its file sections are materialized
// into a fresh work dir: a module seeded with go.mod and go.sum from
// testdata/build unless the archive ships its own. Golden output lives in
// out.txt sections; UPDATE=1 rewrites mismatched goldens in place.
//
// A script is a sequence of commands, one per line:
//
//	exec migrate|go args...      run a command in the work dir
//	cmp actual golden            byte-compare files (actual may be stdout)
//	stdout 'regex'               match the last exec's stdout
//	grep 'regex' file            match a work-dir file
//	stubs v2|v1generic|v1simple  copy shared generated stubs into gen/
//
// A leading ! inverts exec, stdout, and grep. # starts a comment.
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
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("test binary path: %v", err)
	}
	env := scriptEnv(t, scaffold)
	paths, err := filepath.Glob(filepath.Join("testdata", "script", "*.txtar"))
	if err != nil {
		t.Fatalf("glob scripts: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no testdata/script/*.txtar files found")
	}
	for _, path := range paths {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".txtar"), func(t *testing.T) {
			t.Parallel()
			state := &scriptState{
				t:          t,
				path:       path,
				env:        env,
				testBinary: testBinary,
				scaffold:   scaffold,
				repoRoot:   repoRoot,
				update:     os.Getenv("UPDATE") != "",
			}
			state.run()
		})
	}
}

// scriptState is one script's execution state: the materialized work dir, the
// stdout of the most recent exec, and the parsed archive for golden updates.
type scriptState struct {
	t          *testing.T
	path       string
	env        []string
	testBinary string
	scaffold   string
	repoRoot   string
	update     bool

	archive *txtar.Archive
	workDir string
	stdout  []byte
	ranExec bool
	updated bool
}

func (s *scriptState) run() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		s.t.Fatal(err)
	}
	s.archive = txtar.Parse(data)
	s.workDir = s.t.TempDir()
	s.materialize()
	for lineNum, line := range strings.Split(string(s.archive.Comment), "\n") {
		tokens, err := tokenize(line)
		if err != nil {
			s.t.Fatalf("%s:%d: %v", s.path, lineNum+1, err)
		}
		if len(tokens) == 0 {
			continue
		}
		neg := false
		if tokens[0] == "!" {
			neg, tokens = true, tokens[1:]
			if len(tokens) == 0 {
				s.t.Fatalf("%s:%d: ! requires a command", s.path, lineNum+1)
			}
		}
		fail := func(format string, args ...any) {
			s.t.Helper()
			s.t.Fatalf("%s:%d: %s: %s", s.path, lineNum+1, line, fmt.Sprintf(format, args...))
		}
		switch cmd, args := tokens[0], tokens[1:]; cmd {
		case "exec":
			s.cmdExec(fail, neg, args)
		case "cmp":
			s.cmdCmp(fail, neg, args)
		case "stdout":
			s.cmdStdout(fail, neg, args)
		case "grep":
			s.cmdGrep(fail, neg, args)
		case "stubs":
			s.cmdStubs(fail, neg, args)
		default:
			fail("unknown command %q", cmd)
		}
	}
	if s.updated {
		if err := os.WriteFile(s.path, txtar.Format(s.archive), 0o644); err != nil {
			s.t.Fatalf("update %s: %v", s.path, err)
		}
	}
}

// materialize extracts the archive's file sections into the work dir and
// seeds the shared module scaffold unless the case ships its own go.mod
// (nested or self-contained module fixtures manage their own deps). The
// scaffold go.mod's replace target is resolved to this checkout so
// connectrpc.com/connect/v2 builds locally.
func (s *scriptState) materialize() {
	hasGoMod := false
	for _, file := range s.archive.Files {
		if filepath.Base(file.Name) == "go.mod" {
			hasGoMod = true
		}
		dst := filepath.Join(s.workDir, filepath.FromSlash(file.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			s.t.Fatal(err)
		}
		if err := os.WriteFile(dst, file.Data, 0o644); err != nil {
			s.t.Fatal(err)
		}
	}
	if hasGoMod {
		return
	}
	gomod, err := os.ReadFile(filepath.Join(s.scaffold, "go.mod.txt"))
	if err != nil {
		s.t.Fatal(err)
	}
	gomod = []byte(strings.ReplaceAll(string(gomod), "REPLACE_DIR", s.repoRoot))
	if err := os.WriteFile(filepath.Join(s.workDir, "go.mod"), gomod, 0o644); err != nil {
		s.t.Fatal(err)
	}
	if err := copyFile(filepath.Join(s.scaffold, "go.sum"), filepath.Join(s.workDir, "go.sum")); err != nil {
		s.t.Fatal(err)
	}
}

// cmdExec runs `migrate` (this test binary re-exec'd through TestMain) or any
// other program in the work dir, capturing stdout for later assertions.
func (s *scriptState) cmdExec(fail func(string, ...any), neg bool, args []string) {
	if len(args) == 0 {
		fail("usage: exec program [args...]")
	}
	program, env := args[0], s.env
	if program == "migrate" {
		program = s.testBinary
		env = append(append([]string{}, env...), migrateExecEnv+"=1")
	}
	cmd := exec.CommandContext(s.t.Context(), program, args[1:]...)
	cmd.Dir = s.workDir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	s.stdout = stdout.Bytes()
	s.ranExec = true
	if (err != nil) != neg {
		want := "success"
		if neg {
			want = "failure"
		}
		fail("want %s, got %v\nstdout:\n%s\nstderr:\n%s", want, err, stdout.String(), stderr.String())
	}
}

// cmdCmp byte-compares an actual file (or the literal `stdout`) against a
// golden file from the archive. Under UPDATE=1 a mismatched golden's archive
// section is rewritten instead of failing.
func (s *scriptState) cmdCmp(fail func(string, ...any), neg bool, args []string) {
	if neg || len(args) != 2 {
		fail("usage: cmp actual golden")
	}
	actual := s.stdout
	if args[0] != "stdout" {
		var err error
		if actual, err = os.ReadFile(filepath.Join(s.workDir, filepath.FromSlash(args[0]))); err != nil {
			fail("%v", err)
		}
	} else if !s.ranExec {
		fail("stdout requires a prior exec")
	}
	golden, err := os.ReadFile(filepath.Join(s.workDir, filepath.FromSlash(args[1])))
	if err != nil {
		fail("%v", err)
	}
	if bytes.Equal(actual, golden) {
		return
	}
	if s.update {
		for i := range s.archive.Files {
			if s.archive.Files[i].Name == args[1] {
				// txtar sections are always newline-terminated, so an actual
				// without a trailing newline can never round-trip; surface it.
				if len(actual) > 0 && actual[len(actual)-1] != '\n' {
					fail("cannot update golden: output does not end in a newline")
				}
				s.archive.Files[i].Data = actual
				s.updated = true
				return
			}
		}
		fail("cannot update golden: %s is not in the archive", args[1])
	}
	fail("files differ:\n%s", unifiedDiff(args[1], golden, actual, false))
}

func (s *scriptState) cmdStdout(fail func(string, ...any), neg bool, args []string) {
	if len(args) != 1 {
		fail("usage: [!] stdout 'regex'")
	}
	if !s.ranExec {
		fail("stdout requires a prior exec")
	}
	if s.match(fail, args[0], s.stdout) == neg {
		fail("stdout match = %v, want %v\nstdout:\n%s", !neg, neg, s.stdout)
	}
}

func (s *scriptState) cmdGrep(fail func(string, ...any), neg bool, args []string) {
	if len(args) != 2 {
		fail("usage: [!] grep 'regex' file")
	}
	content, err := os.ReadFile(filepath.Join(s.workDir, filepath.FromSlash(args[1])))
	if err != nil {
		fail("%v", err)
	}
	if s.match(fail, args[0], content) == neg {
		fail("%s match = %v, want %v\ncontent:\n%s", args[1], !neg, neg, content)
	}
}

// cmdStubs copies the shared generated stubs for the requested connect version
// into the module's gen/ tree, choosing whether the tool sees v1 or v2 stubs:
//
//	stubs v2         connect v2 stubs (the tool rewrites against them)
//	stubs v1generic  connect v1 stubs, generic form (regenerate-first advice)
//	stubs v1simple   connect v1 stubs, simple form
func (s *scriptState) cmdStubs(fail func(string, ...any), neg bool, args []string) {
	if neg || len(args) != 1 {
		fail("usage: stubs v2|v1generic|v1simple")
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
		fail("unknown stubs %q: want v2, v1generic, or v1simple", args[0])
	}
	const pbRel = "connect/ping/v1/ping.pb.go"
	const connectRel = "connect/ping/v1/pingv1connect/ping.connect.go"
	if err := copyFile(
		filepath.Join(s.scaffold, "gen", pbRel),
		filepath.Join(s.workDir, "gen", pbRel),
	); err != nil {
		fail("%v", err)
	}
	if err := copyFile(
		filepath.Join(s.scaffold, tree, connectRel),
		filepath.Join(s.workDir, "gen", connectRel),
	); err != nil {
		fail("%v", err)
	}
}

// match reports whether the pattern matches content. Patterns compile in
// multiline mode, so ^ and $ anchor per line.
func (s *scriptState) match(fail func(string, ...any), pattern string, content []byte) bool {
	re, err := regexp.Compile("(?m)" + pattern)
	if err != nil {
		fail("bad regexp %q: %v", pattern, err)
	}
	return re.Match(content)
}

// scriptEnv builds the environment for script commands. Scripts run with a
// bare environment plus the Go toolchain's cache and module settings, so go
// build and go/packages resolve offline from the host cache.
func scriptEnv(t *testing.T, scaffold string) []string {
	t.Helper()
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/no-home",
		"SCAFFOLD=" + scaffold,
		// Keep diff output deterministic regardless of how the harness wires
		// stdout: the tool colorizes only without NO_COLOR and on a TTY.
		"NO_COLOR=1",
	}
	for _, name := range []string{
		"GOMODCACHE", "GOCACHE", "GOPATH", "GOPROXY",
		"GOSUMDB", "GOFLAGS", "GOTOOLCHAIN", "GO111MODULE",
	} {
		if val, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+val)
		} else if val := goEnv(t.Context(), name); val != "" {
			env = append(env, name+"="+val)
		}
	}
	return env
}

// tokenize splits a script line into fields. A field may be single-quoted,
// with ” inside quotes reading as a literal quote; # starts a comment.
// Environment expansion is not supported, so $ in a token is an error.
func tokenize(line string) ([]string, error) {
	var tokens []string
	rest := strings.TrimSpace(line)
	for rest != "" {
		var token string
		switch rest[0] {
		case '#':
			return tokens, nil
		case '\'':
			body := rest[1:]
			var builder strings.Builder
			for {
				closing := strings.IndexByte(body, '\'')
				if closing < 0 {
					return nil, errors.New("unterminated quote")
				}
				builder.WriteString(body[:closing])
				body = body[closing+1:]
				if !strings.HasPrefix(body, "'") {
					break
				}
				builder.WriteByte('\'')
				body = body[1:]
			}
			token, rest = builder.String(), body
		default:
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				end = len(rest)
			}
			token, rest = rest[:end], rest[end:]
			if strings.Contains(token, "$") {
				return nil, fmt.Errorf("environment expansion is not supported: %q", token)
			}
			if strings.Contains(token, "'") {
				return nil, fmt.Errorf("quotes must start a token: %q", token)
			}
		}
		tokens = append(tokens, token)
		rest = strings.TrimLeft(rest, " \t")
	}
	return tokens, nil
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
