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

// Command connect-go-v2-migrate rewrites Go source code to migrate from
// connectrpc.com/connect to connectrpc.com/connect/v2. It applies AST
// transformations to update v1 code automatically, emitting warnings for
// patterns that require manual intervention.
//
// Usage: connect-go-v2-migrate [-w] [-json] [paths...]. Paths default to the
// current directory. Without -w the tool is a dry run that prints diffs.
package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const migratingGuideURL = "https://github.com/connectrpc/connect-go/blob/main/docs/v2-migration.md"

func usage(flags *flag.FlagSet) {
	out := flags.Output()
	fmt.Fprint(out, `connect-go-v2-migrate rewrites Go code from connectrpc.com/connect (v1) to
connectrpc.com/connect/v2, applying the mechanical parts of the migration:

  - unwraps *connect.Request[T] and *connect.Response[T] in handler and client
    signatures
  - removes .Msg access and connect.NewRequest or connect.NewResponse wrappers
  - converts connect.NewError while preserving the v1 wire message
  - adds context.Context to streaming Send and Receive calls
  - updates connect-go Buf options for v2

The tool reports warnings for code that needs a manual v2 update.

It loads Go packages to find files that use connect directly or through
generated stubs. It also scans Buf templates named buf.gen.yaml or
buf.gen.*.yaml. Generated code is never edited. This includes files with a
"DO NOT EDIT" header and files under a buf.gen.yaml out directory.

Migration runs in two phases. When v1 generated code is present, the tool only
updates Buf templates and prints the steps to generate v2 bindings. Run it
again after generation to rewrite Go call sites.

usage: connect-go-v2-migrate [-w] [-json] [paths...]

Paths may be files or directories. They default to the current directory (".").
By default the tool is a dry run that prints unified diffs for changed files.
Pass -w to write changes to disk.

Flags:
`)
	flags.PrintDefaults()
	fmt.Fprintf(out, "\nFull migration guide: %s\n", migratingGuideURL)
}

func main() {
	os.Exit(runMain(os.Args[1:]))
}

func runMain(args []string) int {
	flags := flag.NewFlagSet("connect-go-v2-migrate", flag.ContinueOnError)
	write := flags.Bool("w", false, "write rewrites back to disk (default: dry-run, print diffs).")
	jsonOut := flags.Bool("json", false, "emit a structured JSON report instead of text.")
	flags.Usage = func() { usage(flags) }
	if err := flags.Parse(args); err != nil {
		return 2
	}

	roots := flags.Args()
	if len(roots) == 0 {
		roots = []string{"."}
	}

	proj, err := discover(roots)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discover: %v\n", err)
		return 1
	}

	run := results{scanned: proj.goFilesScanned + len(proj.templates)}
	for _, template := range proj.templates {
		processFile(template, RewriteBufGen, *write, &run)
	}
	// Generator-install guidance drives the phase-1 steps, not code diagnostics.
	run.installNotes, run.diagnostics = splitInstallNotes(run.diagnostics)

	// Migration is per stub package: a source whose stubs are already v2 is
	// rewritten now; one still binding a v1 stub has its stub-dependent rewrites
	// deferred until that stub is regenerated.
	anyReady := false
	for _, source := range proj.sources {
		if source.ready {
			anyReady = true
			break
		}
	}
	// Pure regenerate-first (v1 stubs, nothing migratable yet) skips the sources.
	if anyReady || !proj.hasV1Gen {
		for _, source := range proj.sources {
			ready := source.ready
			rewrite := func(path string, content []byte) ([]byte, Report, error) {
				return Rewrite(path, content, ready, withHandlerStreams(proj.handlerStreams))
			}
			processFile(source, rewrite, *write, &run)
		}
	}
	if anyReady && run.flippedResult {
		// Type-directed post-pass over the rewritten overlay: strip the .Msg
		// selectors a return-type flip left dangling, including in _test.go files.
		// Skipped when no return type flipped, since nothing can dangle then.
		stripDanglingMsgPass(roots, *write, &run)
	}

	// Sort warnings so the reported diagnostics are deterministic.
	slices.SortStableFunc(run.diagnostics, func(left, right Diagnostic) int {
		return cmp.Or(
			cmp.Compare(left.File, right.File),
			cmp.Compare(left.Line, right.Line),
			cmp.Compare(left.Column, right.Column),
			cmp.Compare(left.Rule, right.Rule),
			cmp.Compare(left.Message, right.Message),
		)
	})

	printReport(&run, &proj, *write, *jsonOut, anyReady, wantColor())
	if run.errored > 0 {
		return 1
	}
	return 0
}

// rewriteResult is a file the tool changed, kept for the REWRITES section.
type rewriteResult struct {
	path    string
	src     []byte
	out     []byte
	summary string
}

const (
	categoryManual   = "manual_update"
	categoryDeferred = "deferred_update"
)

// Diagnostic is one issue for the user, with a display path and a stable rule.
type Diagnostic struct {
	Category string `json:"category"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Message  string `json:"message"`
	Rule     string `json:"rule"`
}

// results accumulates a whole run so the output can be grouped into sections.
type results struct {
	scanned     int
	rewrites    []rewriteResult
	diagnostics []Diagnostic
	// installNotes are generator-install diagnostics that drive the phase-1
	// steps instead of being reported as code issues.
	installNotes []Diagnostic
	// flippedResult is set when a rewrite unwrapped a response return type,
	// the only edit that can leave a caller's .Msg dangling in another file.
	flippedResult bool
	errored       int
}

func splitInstallNotes(diagnostics []Diagnostic) (notes, rest []Diagnostic) {
	for _, diag := range diagnostics {
		if diag.Rule == ruleBufgenReinstall || diag.Rule == ruleBufgenGoMod {
			notes = append(notes, diag)
			continue
		}
		rest = append(rest, diag)
	}
	return notes, rest
}

// processFile rewrites one file and accumulates its outcome into run, writing
// to disk when -w is set. Printing is deferred to printReport.
func processFile(file fileContent, rewrite func(string, []byte) ([]byte, Report, error), write bool, run *results) {
	out, report, err := rewrite(file.path, file.content)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: rewrite: %v\n", file.path, err)
		run.errored++
		return
	}
	if report.Counts["result_unwrap_response"] > 0 {
		run.flippedResult = true
	}
	if report.Changed {
		// A failed write must not be reported as applied.
		recorded := true
		if write {
			if err := os.WriteFile(file.path, out, 0o644); err != nil { //nolint:gosec // rewriting Go source files, world-readable matches typical repo perms
				fmt.Fprintf(os.Stderr, "%s: write: %v\n", file.path, err)
				run.errored++
				recorded = false
			}
		}
		if recorded {
			run.rewrites = append(run.rewrites, rewriteResult{
				path: file.path, src: file.content, out: out, summary: report.Summary(),
			})
		}
	}
	for _, warning := range report.Warnings {
		run.diagnostics = append(run.diagnostics, toDiagnostic(file.path, warning))
	}
}

// stripDanglingMsgPass runs the type-directed .Msg post-pass over an overlay of
// the per-file rewrites and merges its edits into run, updating an existing
// rewrite or adding one for a file (often a _test.go) the pass never touched.
func stripDanglingMsgPass(roots []string, write bool, run *results) {
	overlay := map[string][]byte{}
	for _, rewrite := range run.rewrites {
		if strings.HasSuffix(rewrite.path, ".go") {
			overlay[rewrite.path] = rewrite.out
		}
	}
	edits, err := stripDanglingMsg(roots, overlay)
	if err != nil {
		// Non-fatal: the per-file rewrites stand even if the post-pass can't
		// re-type-check an incomplete migration.
		fmt.Fprintf(os.Stderr, "strip dangling .Msg: %v\n", err)
		return
	}
	for path, edit := range edits {
		if write {
			if err := os.WriteFile(path, edit.content, 0o644); err != nil { //nolint:gosec // rewriting Go source files, world-readable matches typical repo perms
				fmt.Fprintf(os.Stderr, "%s: write: %v\n", path, err)
				run.errored++
				continue
			}
		}
		mergeMsgEdit(run, path, edit)
	}
}

// mergeMsgEdit folds one .Msg edit into the run, replacing an existing
// rewrite's output or recording a fresh one.
func mergeMsgEdit(run *results, path string, edit msgEdit) {
	for i := range run.rewrites {
		if run.rewrites[i].path == path {
			run.rewrites[i].out = edit.content
			run.rewrites[i].summary += fmt.Sprintf(" strip_dangling_msg=%d", edit.count)
			return
		}
	}
	run.rewrites = append(run.rewrites, rewriteResult{
		path: path, src: edit.base, out: edit.content,
		summary: fmt.Sprintf("strip_dangling_msg=%d", edit.count),
	})
}

// toDiagnostic converts a Warning into a Diagnostic, falling back to the
// processed file when the warning is position-less.
func toDiagnostic(fallback string, warning Warning) Diagnostic {
	file := warning.File
	if file == "" {
		file = fallback
	}
	category := categoryManual
	if warning.Kind == WarningDeferred {
		category = categoryDeferred
	}
	return Diagnostic{
		Category: category,
		File:     displayPath(file),
		Line:     warning.Line,
		Column:   warning.Col,
		Message:  warning.Msg,
		Rule:     warning.Rule,
	}
}

func printReport(run *results, proj *project, write, jsonOut, migrated, color bool) {
	if jsonOut {
		printJSON(run, proj, write)
		return
	}
	printText(run, proj, write, migrated, color)
}

func wantColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// printText writes the diffs and diagnostics, then a footer with the scan
// summary (last on purpose, so the counts survive a long scrollback). With v1
// stubs present and nothing migratable it is the regenerate-first report.
func printText(run *results, proj *project, write, migrated, color bool) {
	if proj.hasV1Gen && !migrated {
		printPhase1Text(run, proj, write, color)
		return
	}

	body := false
	if len(run.rewrites) > 0 {
		lead := "Proposed rewrites (rerun with -w to apply):"
		if write {
			lead = "Applied rewrites:"
		}
		fmt.Printf("%s\n", lead)
		for _, rewrite := range run.rewrites {
			fmt.Printf("  %s: %s\n", displayPath(rewrite.path), rewrite.summary)
			if !write {
				fmt.Println(unifiedDiff(displayPath(rewrite.path), rewrite.src, rewrite.out, color))
			}
		}
		body = true
	}

	if manual := byCategory(run.diagnostics, categoryManual); len(manual) > 0 {
		if body {
			fmt.Println()
		}
		fmt.Print("The following issues require manual code changes:\n")
		for _, diag := range manual {
			fmt.Printf("  %s\n", formatDiagnostic(diag))
		}
		body = true
	}

	if deferred := byCategory(run.diagnostics, categoryDeferred); len(deferred) > 0 {
		if body {
			fmt.Println()
		}
		fmt.Print("Deferred until their connect stubs are regenerated to v2:\n")
		for _, diag := range deferred {
			fmt.Printf("  %s\n", formatDiagnostic(diag))
		}
		fmt.Print("Regenerate those stubs (buf generate) and re-run connect-go-v2-migrate.\n")
		body = true
	}

	// Mocks aren't connect stubs, their own tool regenerates them on v2.
	if len(proj.mockV1Pkgs) > 0 {
		if body {
			fmt.Println()
		}
		fmt.Print("Generated mocks still import connect v1. Regenerate them after migrating:\n")
		for _, pkg := range proj.mockV1Pkgs {
			fmt.Printf("  %s\n", pkg)
		}
		body = true
	}

	if body {
		fmt.Println()
	}
	fmt.Printf("%s %s\n", scanCounts(proj), rewriteStatus(len(run.rewrites), write))

	// Explain a zero result: ran outside a module, or found no connect usage.
	switch {
	case proj.packagesLoaded == 0:
		for _, line := range noGoPackagesLines(proj) {
			fmt.Println(line)
		}
	case proj.goFilesScanned > 0 && len(proj.sources) == 0 && len(run.rewrites) == 0 && len(run.diagnostics) == 0:
		fmt.Println("None of the scanned Go files import connectrpc.com/connect, so there is nothing to migrate.")
	}

	// The migrated code needs the v2 modules in go.mod; prompt for any missing.
	if missing := missingV2ModulesAdvice(run, proj); len(missing) > 0 {
		fmt.Print("\ngo.mod is missing the v2 modules. Pull them in and tidy:\n\n")
		fmt.Printf("  %s\n", goGetCommand(missing))
		fmt.Print("  go mod tidy\n")
	}

	if len(run.rewrites) > 0 || len(run.diagnostics) > 0 {
		fmt.Printf("Full migration guide: %s\n", migratingGuideURL)
	}
}

// printPhase1Text renders the regenerate-first report: while the generated code
// targets v1, the only edits are Buf template updates, and the report walks the
// user through switching generation to v2 and re-running.
func printPhase1Text(run *results, proj *project, write, color bool) {
	summary := scanCounts(proj) + " The generated Connect code still targets v1, so no Go source changes are proposed yet."
	for _, line := range wrapLines(summary, 78) {
		fmt.Println(line)
	}

	// The main module's own generated code is regenerated locally.
	if len(proj.v1GenDirs) > 0 {
		fmt.Print("\nGenerated v1 Connect code:\n")
		for _, dir := range proj.v1GenDirs {
			fmt.Printf("  %s\n", displayPath(dir))
		}
	}
	// BSR-generated SDK dependencies are updated through go get, not regenerated.
	if len(proj.sdkModules) > 0 {
		fmt.Print("\nGenerated v1 Connect SDKs (the go get @v2 below updates these):\n")
		for _, mod := range proj.sdkModules {
			fmt.Printf("  %s\n", mod)
		}
	}
	// Other dependencies shipping v1 connect code have no reliable version query.
	if len(proj.externalGenModules) > 0 {
		fmt.Print("\nDependencies shipping v1 Connect code (update each to a connect-v2 build):\n")
		for _, mod := range proj.externalGenModules {
			fmt.Printf("  %s\n", mod)
		}
	}

	if len(run.rewrites) > 0 {
		lead := "Proposed Buf template updates (rerun with -w to apply):"
		if write {
			lead = "Applied Buf template updates:"
		}
		fmt.Printf("\n%s\n", lead)
		for _, rewrite := range run.rewrites {
			fmt.Printf("  %s: %s\n", displayPath(rewrite.path), rewrite.summary)
			if !write {
				fmt.Println(unifiedDiff(displayPath(rewrite.path), rewrite.src, rewrite.out, color))
			}
		}
	}

	fmt.Print("\nFirst, move the dependencies and generated code to v2:\n\n")
	for number, step := range phase1Steps(run, proj, write) {
		fmt.Printf("  %d. %s\n", number+1, step.cmd)
		if step.note != "" {
			for _, line := range wrapLines("("+step.note+")", 72) {
				fmt.Printf("     %s\n", line)
			}
		}
	}
	fmt.Print("\nThen re-run connect-go-v2-migrate to work through the Go source changes: it\nrewrites the call sites against the v2 stubs and reports anything that needs\na manual update.\n")
	fmt.Printf("\nFull migration guide: %s\n", migratingGuideURL)
}

// wrapLines greedily wraps text on spaces so no line exceeds width.
func wrapLines(text string, width int) []string {
	var lines []string
	line := ""
	for word := range strings.FieldsSeq(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// step is one numbered action in the phase-1 switch-to-v2 instructions.
type step struct {
	cmd  string
	note string
}

// phase1Steps builds the move-to-v2 instructions: a `go get -u` for the v2 core,
// SDK dependencies (@v2), and ecosystem modules, plus local regeneration steps
// when the main module generates its own connect code.
func phase1Steps(run *results, proj *project, write bool) []step {
	var steps []step
	if !write && len(run.rewrites) > 0 {
		steps = append(steps, step{cmd: "connect-go-v2-migrate -w  (applies the Buf template update above)"})
	}
	steps = append(steps, step{
		cmd:  goGetCommand(goGetModules(proj)),
		note: "pulls the v2 core, generated SDKs, and ecosystem modules into go.mod",
	})
	if len(proj.v1GenDirs) > 0 {
		steps = append(steps, localGenSteps(run)...)
	}
	return steps
}

// goGetModules is the v2 module set to `go get -u`: the connect core, every SDK
// dependency at @v2, and the ecosystem modules.
func goGetModules(proj *project) []string {
	mods := make([]string, 0, 1+len(proj.sdkModules)+len(proj.ecosystemModules))
	mods = append(mods, connectV2Module)
	for _, mod := range proj.sdkModules {
		mods = append(mods, mod+"@v2")
	}
	return append(mods, proj.ecosystemModules...)
}

// missingV2Modules returns the v2 modules from goGetModules that the main
// module's go.mod does not require yet. Reports nothing when no go.mod was
// parsed rather than guessing.
func missingV2Modules(proj *project) []string {
	if proj.goModRequires == nil {
		return nil
	}
	var missing []string
	for _, mod := range goGetModules(proj) {
		path, isSDK := strings.CutSuffix(mod, "@v2")
		version, ok := proj.goModRequires[path]
		if !ok || (isSDK && !strings.HasPrefix(version, "v2.")) {
			missing = append(missing, mod)
		}
	}
	return missing
}

// missingV2ModulesAdvice returns the modules a `go get` hint should name, or
// nil when there is no connect work in scope or go.mod already has them all.
func missingV2ModulesAdvice(run *results, proj *project) []string {
	if len(run.rewrites) == 0 && len(run.diagnostics) == 0 && len(proj.sources) == 0 {
		return nil
	}
	return missingV2Modules(proj)
}

// goGetCommand renders a copy-pasteable `go get -u`, one module per line.
func goGetCommand(mods []string) string {
	var builder strings.Builder
	builder.WriteString("go get -u")
	for _, mod := range mods {
		builder.WriteString(" \\\n       ")
		builder.WriteString(mod)
	}
	return builder.String()
}

// localGenSteps are the regeneration steps for a project that generates its own
// connect code: switch the plugin to v2 and regenerate.
func localGenSteps(run *results) []step {
	var steps []step
	seen := map[string]bool{}
	for _, note := range run.installNotes {
		if seen[note.Rule] {
			continue
		}
		seen[note.Rule] = true
		switch note.Rule {
		case ruleBufgenReinstall:
			steps = append(steps, step{
				cmd:  fmt.Sprintf("go install %s/cmd/%s@latest", connectV2Module, connectLocalPlugin),
				note: fmt.Sprintf("%s runs the local %s binary. v1 and v2 share the binary name, so reinstalling from the /v2 module switches generation to v2", note.File, connectLocalPlugin),
			})
		case ruleBufgenGoMod:
			steps = append(steps, step{
				cmd:  fmt.Sprintf("go get -tool %s/cmd/%s && go mod tidy", connectV2Module, connectLocalPlugin),
				note: note.File + " runs the plugin through go.mod. The template entry stays the same",
			})
		}
	}
	if len(run.installNotes) == 0 && len(run.rewrites) == 0 {
		steps = append(steps, step{
			cmd:  fmt.Sprintf("go install %s/cmd/%s@latest", connectV2Module, connectLocalPlugin),
			note: "see docs/v2-migration.md if you generate with a remote plugin or a go.mod tool",
		})
	}
	return append(steps, step{cmd: "buf generate"})
}

// jsonReport is the schema emitted by the -json flag.
//
//nolint:tagliatelle // snake_case is the published JSON schema
type jsonReport struct {
	Summary          jsonSummary  `json:"summary"`
	Diagnostics      []Diagnostic `json:"diagnostics"`
	BufTemplates     []string     `json:"buf_templates,omitempty"`
	IgnoredPaths     []string     `json:"ignored_paths,omitempty"`
	NextSteps        []string     `json:"next_steps,omitempty"`
	DocumentationURL string       `json:"documentation_url,omitempty"`
}

//nolint:tagliatelle // snake_case is the published JSON schema
type jsonSummary struct {
	FilesScanned         int `json:"files_scanned"`
	RewritesApplied      int `json:"rewrites_applied"`
	FilesNeedingFollowUp int `json:"files_needing_follow_up"`
}

func printJSON(run *results, proj *project, write bool) {
	report := jsonReport{
		Summary: jsonSummary{
			FilesScanned:         run.scanned,
			RewritesApplied:      len(run.rewrites),
			FilesNeedingFollowUp: distinctFiles(run.diagnostics),
		},
		Diagnostics:      run.diagnostics,
		DocumentationURL: migratingGuideURL,
	}
	if report.Diagnostics == nil {
		report.Diagnostics = []Diagnostic{}
	}
	for _, template := range proj.templates {
		report.BufTemplates = append(report.BufTemplates, displayPath(template.path))
	}
	missing := missingV2ModulesAdvice(run, proj)
	switch {
	case proj.hasV1Gen:
		// Regenerate-first phase: name the ignored v1 trees and the move-to-v2 steps.
		// Display-relative, matching the text report and keeping output stable.
		for _, dir := range proj.v1GenDirs {
			report.IgnoredPaths = append(report.IgnoredPaths, displayPath(dir))
		}
		for _, step := range phase1Steps(run, proj, write) {
			report.NextSteps = append(report.NextSteps, step.cmd)
		}
		report.NextSteps = append(report.NextSteps, "re-run connect-go-v2-migrate")
	case len(missing) > 0:
		report.NextSteps = append(report.NextSteps, goGetCommand(missing), "go mod tidy")
	case proj.packagesLoaded == 0:
		// No Go module loaded: surface the nested modules to run inside.
		for _, dir := range proj.nestedModuleDirs {
			report.NextSteps = append(report.NextSteps, fmt.Sprintf("cd %s && connect-go-v2-migrate", displayPath(dir)))
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false) // keep "->" and quotes literal in messages
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "encode json: %v\n", err)
	}
}

func byCategory(diagnostics []Diagnostic, category string) []Diagnostic {
	var out []Diagnostic
	for _, diag := range diagnostics {
		if diag.Category == category {
			out = append(out, diag)
		}
	}
	return out
}

func distinctFiles(diagnostics []Diagnostic) int {
	seen := map[string]bool{}
	for _, diag := range diagnostics {
		seen[diag.File] = true
	}
	return len(seen)
}

// scanCounts renders the "Scanned N Go file(s) and M Buf template(s)." prefix,
// keeping the two input kinds distinct.
func scanCounts(proj *project) string {
	return fmt.Sprintf("Scanned %s and %s.",
		plural(proj.goFilesScanned, "Go file"),
		plural(len(proj.templates), "Buf template"))
}

func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

// noGoPackagesLines explains why no Go packages loaded, naming any nested
// modules since "./..." stops at module boundaries.
func noGoPackagesLines(proj *project) []string {
	if len(proj.nestedModuleDirs) > 0 {
		lines := make([]string, 0, 2+len(proj.nestedModuleDirs))
		lines = append(lines,
			`No Go packages were loaded. "./..." does not cross module boundaries, and`,
			"each directory below has its own go.mod, so run the tool from inside them:")
		for _, dir := range proj.nestedModuleDirs {
			lines = append(lines, fmt.Sprintf("  cd %s && connect-go-v2-migrate", displayPath(dir)))
		}
		return lines
	}
	return []string{"No Go packages were loaded. Run connect-go-v2-migrate from inside a Go module (a directory with a go.mod)."}
}

func rewriteStatus(count int, write bool) string {
	switch {
	case count == 0:
		return "No automatic rewrites were applied."
	case write:
		return fmt.Sprintf("%d rewrite(s) applied.", count)
	default:
		return fmt.Sprintf("%d rewrite(s) ready (rerun with -w to apply).", count)
	}
}

// formatDiagnostic renders a diagnostic in file:line:col: format.
func formatDiagnostic(diag Diagnostic) string {
	if diag.Line > 0 {
		return fmt.Sprintf("%s:%d:%d: %s", diag.File, diag.Line, diag.Column, diag.Message)
	}
	return fmt.Sprintf("%s: %s", diag.File, diag.Message)
}

// displayPath renders a path relative to the working directory with a leading
// "./". Absolute paths outside the working directory are left as-is.
func displayPath(path string) string {
	if path == "" {
		return path
	}
	if filepath.IsAbs(path) {
		if cwd, err := os.Getwd(); err == nil {
			if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
				path = rel
			}
		}
	}
	if filepath.IsAbs(path) {
		return path
	}
	// Forward slashes keep output stable across platforms.
	path = filepath.ToSlash(path)
	if strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") {
		return path
	}
	return "./" + path
}
