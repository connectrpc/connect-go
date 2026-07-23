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
	"path"
	"strings"
)

const (
	connectV2Module       = "connectrpc.com/connect/v2"
	connectLocalPlugin    = "protoc-gen-connect-go"
	connectRemotePlugin   = "buf.build/connectrpc/go"
	connectRemotePluginV2 = connectRemotePlugin + ":v2.0.0"
)

// Plugin entry kinds returned by connectPluginItem.
const (
	kindLocal  = "local"  // a local binary on $PATH
	kindGotool = "gotool" // a `go tool`/`go run` command resolved through go.mod
	kindRemote = "remote" // a buf.build remote plugin
)

// isBufGenFile reports whether base names a Buf generation template
// (buf.gen.yaml, buf.gen.yml, or a buf.gen.<name>.yaml variant).
func isBufGenFile(base string) bool {
	if !strings.HasSuffix(base, ".yaml") && !strings.HasSuffix(base, ".yml") {
		return false
	}
	return base == "buf.gen.yaml" || base == "buf.gen.yml" || strings.HasPrefix(base, "buf.gen.")
}

// RewriteBufGen applies the v1->v2 buf.gen.yaml migration: strip the v1
// `simple` option from any connect-go plugin entry, pin a remote plugin to the
// v2 release, and warn when a local plugin needs reinstalling from /v2. Other
// lines are left untouched; the returned error is always nil.
func RewriteBufGen(filename string, src []byte) ([]byte, Report, error) {
	report := Report{}
	// Edits are keyed by line index and applied in one final pass, so block
	// boundaries stay valid throughout the scan; `lines` is never mutated.
	lines := strings.Split(string(src), "\n")
	edits := lineEdits{replace: map[int]string{}, delete: map[int]bool{}}

	for index := 0; index < len(lines); index++ {
		kind, ref, ok := connectPluginItem(lines[index])
		if !ok {
			continue
		}
		// The block runs until a sibling item or a dedent to the dash's indent.
		dashIndent := indentOf(lines[index])
		end := index + 1
		for end < len(lines) {
			if line := lines[end]; strings.TrimSpace(line) != "" && indentOf(line) <= dashIndent {
				break
			}
			end++
		}
		stripSimpleOpt(lines, index+1, end, &edits, &report)
		if kind == kindLocal {
			// A v1 `path:` override can reroute through `go run`/`go tool`.
			if pathKind, pathRef, ok := pathOverride(lines, index+1, end); ok {
				kind, ref = pathKind, pathRef
			}
		}
		switch kind {
		case kindLocal:
			report.warnAtLinef(filename, index+1, ruleBufgenReinstall, "reinstall the generator with `go install %s/cmd/%s@latest`. The v1 and v2 plugins share the binary name %q, so reinstalling from the /v2 module switches generation to v2.", connectV2Module, connectLocalPlugin, connectLocalPlugin)
		case kindGotool:
			report.warnAtLinef(filename, index+1, ruleBufgenGoMod, "the plugin runs via go.mod (%s). Update the tool dependency to the v2 module with `go get -tool %s/cmd/%s` then `go mod tidy`. The buf.gen.yaml entry stays the same.", ref, connectV2Module, connectLocalPlugin)
		case kindRemote:
			pinRemotePluginV2(lines, index, ref, &edits, &report)
		}
		index = end - 1
	}

	if !report.Changed {
		return src, report, nil
	}
	return []byte(strings.Join(edits.apply(lines), "\n")), report, nil
}

// pinRemotePluginV2 pins a v1 remote plugin reference (versioned or not) to
// the v2 release. References already at v2 are left alone.
func pinRemotePluginV2(lines []string, index int, ref string, edits *lineEdits, report *Report) {
	if strings.HasPrefix(ref, connectRemotePlugin+":v2") {
		return
	}
	edits.replace[index] = strings.Replace(lines[index], ref, connectRemotePluginV2, 1)
	report.bump("bufgen_pin_remote_v2")
}

// lineEdits records pending line replacements or deletions keyed by line index.
type lineEdits struct {
	replace map[int]string
	delete  map[int]bool
}

// apply produces the edited line slice: deletions dropped, replacements
// substituted, the rest copied verbatim.
func (e lineEdits) apply(lines []string) []string {
	out := make([]string, 0, len(lines))
	for index, line := range lines {
		if e.delete[index] {
			continue
		}
		if replacement, ok := e.replace[index]; ok {
			out = append(out, replacement)
			continue
		}
		out = append(out, line)
	}
	return out
}

// connectPluginItem reports whether line is a plugins sequence item naming the
// connect-go generator, returning its kind and raw reference. It recognises the
// v2 (`local:`/`remote:`) and v1 (`name:`/`plugin:`) syntaxes; a `local:` flow
// sequence routed through `go run`/`go tool` is reported as kindGotool.
func connectPluginItem(line string) (kind, ref string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "- ") {
		return "", "", false
	}
	rest := strings.TrimSpace(trimmed[len("- "):])
	switch {
	case strings.HasPrefix(rest, "local:"):
		return classifyLocalPlugin(yamlScalar(rest[len("local:"):]))
	case strings.HasPrefix(rest, "remote:"):
		if value := yamlScalar(rest[len("remote:"):]); isConnectRemoteRef(value) {
			return kindRemote, value, true
		}
	case strings.HasPrefix(rest, "name:"): // v1 syntax
		if value := yamlScalar(rest[len("name:"):]); value == "connect-go" {
			return kindLocal, value, true
		}
	case strings.HasPrefix(rest, "plugin:"): // v1 syntax: local name or remote ref
		value := yamlScalar(rest[len("plugin:"):])
		switch {
		case isConnectRemoteRef(value):
			return kindRemote, value, true
		case value == "connect-go" || value == connectLocalPlugin:
			return kindLocal, value, true
		}
	}
	return "", "", false
}

// isConnectRemoteRef reports whether value references the connect-go remote
// plugin, bare or version-tagged.
func isConnectRemoteRef(value string) bool {
	return value == connectRemotePlugin || strings.HasPrefix(value, connectRemotePlugin+":")
}

// pathOverride scans a v1 plugin block for a `path:` whose value is a command
// (YAML flow sequence) naming the connect-go generator. A plain string path
// keeps the entry a local binary.
func pathOverride(lines []string, start, end int) (kind, ref string, ok bool) {
	for index := start; index < end; index++ {
		trimmed := strings.TrimSpace(lines[index])
		if !strings.HasPrefix(trimmed, "path:") {
			continue
		}
		return classifyLocalPlugin(yamlScalar(trimmed[len("path:"):]))
	}
	return "", "", false
}

// classifyLocalPlugin classifies a `local:` value (bare binary name or a flow
// sequence command) as the connect-go plugin. A `go tool`/`go run` indirection
// is reported as kindGotool.
func classifyLocalPlugin(value string) (kind, ref string, ok bool) {
	value = strings.TrimSpace(value)
	if value == connectLocalPlugin {
		return kindLocal, value, true
	}
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return "", "", false
	}
	command := splitFlowSequence(value)
	if len(command) == 0 || path.Base(command[len(command)-1]) != connectLocalPlugin {
		return "", "", false
	}
	if len(command) >= 2 && command[0] == "go" && (command[1] == "tool" || command[1] == "run") {
		return kindGotool, value, true
	}
	return kindLocal, value, true
}

// splitFlowSequence parses a YAML flow sequence ("[a, b, c]") into trimmed,
// unquoted elements.
func splitFlowSequence(value string) []string {
	inner := strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	var elements []string
	for part := range strings.SplitSeq(inner, ",") {
		if trimmed := yamlScalar(part); trimmed != "" {
			elements = append(elements, trimmed)
		}
	}
	return elements
}

// stripSimpleOpt removes the v1 `simple` option from the plugin block in
// [start, end), handling both the inline (opt: a,simple=true) and list (opt:
// with `- simple=true` items) forms.
func stripSimpleOpt(lines []string, start, end int, edits *lineEdits, report *Report) {
	for index := start; index < end; index++ {
		optIndent, value, isOpt := optLine(lines[index])
		if !isOpt {
			continue
		}
		if value != "" {
			stripInlineSimple(index, optIndent, value, edits, report)
			return
		}
		stripListSimple(lines, index, optIndent, end, edits, report)
		return
	}
}

// stripInlineSimple rewrites `opt: a,simple=x,b` to `opt: a,b`, or deletes the
// whole line when `simple` was the only option.
func stripInlineSimple(index, indent int, value string, edits *lineEdits, report *Report) {
	kept := make([]string, 0)
	dropped := false
	for token := range strings.SplitSeq(value, ",") {
		if isSimpleToken(token) {
			dropped = true
			continue
		}
		kept = append(kept, strings.TrimSpace(token))
	}
	if !dropped {
		return
	}
	report.bump("bufgen_remove_simple")
	if len(kept) == 0 {
		edits.delete[index] = true
		return
	}
	edits.replace[index] = strings.Repeat(" ", indent) + "opt: " + strings.Join(kept, ",")
}

// stripListSimple removes `- simple=x` entries from an opt list and, if that
// empties the list, the `opt:` header too.
func stripListSimple(lines []string, optIndex, optIndent, end int, edits *lineEdits, report *Report) {
	var itemCount, simpleCount int
	simpleLines := make([]int, 0)
	for index := optIndex + 1; index < end; index++ {
		line := lines[index]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if indentOf(line) <= optIndent {
			break // dedented out of the opt list
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(trimmed, "- ") {
			break // not a list item, opt block ended
		}
		itemCount++
		if isSimpleToken(yamlScalar(trimmed[len("- "):])) {
			simpleCount++
			simpleLines = append(simpleLines, index)
		}
	}
	if simpleCount == 0 {
		return
	}
	report.bump("bufgen_remove_simple")
	for _, index := range simpleLines {
		edits.delete[index] = true
	}
	if simpleCount == itemCount {
		// Every option was `simple`. Drop the now-empty `opt:` header too.
		edits.delete[optIndex] = true
	}
}

// optLine reports whether line is an `opt:` key. It returns the key's
// indentation and the inline value (empty for the list form `opt:`).
func optLine(line string) (indent int, value string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "opt:") {
		return 0, "", false
	}
	return indentOf(line), yamlScalar(trimmed[len("opt:"):]), true
}

// isSimpleToken reports whether a single opt token is the v1 `simple` flag,
// with or without a value (simple, simple=true, simple=false).
func isSimpleToken(token string) bool {
	token = strings.TrimSpace(token)
	return token == "simple" || strings.HasPrefix(token, "simple=")
}

// yamlScalar trims whitespace, surrounding quotes, and any trailing line
// comment from a scalar value. A quoted value is returned verbatim so a "#"
// inside the quotes is not mistaken for a comment.
func yamlScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
		if end := strings.IndexByte(value[1:], value[0]); end >= 0 {
			return value[1 : 1+end]
		}
	}
	if hash := strings.Index(value, " #"); hash >= 0 {
		value = strings.TrimSpace(value[:hash])
	}
	return value
}

// indentOf counts leading spaces. YAML forbids tabs for indentation, so a tab
// stops the count, which is fine for our purposes.
func indentOf(line string) int {
	count := 0
	for count < len(line) && line[count] == ' ' {
		count++
	}
	return count
}
