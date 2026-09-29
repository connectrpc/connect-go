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
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	connectV2Module    = "connectrpc.com/connect/v2"
	connectLocalPlugin = "protoc-gen-connect-go"
)

// connectRemotePluginRef matches connect-go remotes plugins.
var connectRemotePluginRef = regexp.MustCompile(`^(` + bsrHostPattern + `)/connectrpc/(go|gosimple)(?::(\S+))?$`)

// Plugin entry kinds returned by findConnectPlugin.
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

// RewriteBufGen migrates a buf.gen.yaml to the v2 plugin. It edits the original
// lines instead of re-encoding the YAML, so formatting and comments are kept.
func RewriteBufGen(filename string, src []byte) ([]byte, Report, error) {
	report := Report{}
	lines := strings.Split(string(src), "\n")
	entries, err := parsePluginEntries(src)
	if err != nil {
		report.warnAtLinef(filename, 1, ruleBufgenManualEdit, "could not parse the template, so it was not migrated: %v", err)
		return src, report, nil
	}
	edits := &lineEdits{lines: lines, replace: map[int]string{}, delete: map[int]bool{}}
	for _, entry := range entries {
		plugin, ok := findConnectPlugin(entry)
		if !ok {
			continue
		}
		stripSimpleOpt(filename, plugin, edits, &report)
		line := plugin.key().Line
		switch plugin.kind {
		case kindLocal:
			report.warnAtLinef(filename, line, ruleBufgenReinstall, "reinstall the generator with `go install %s/cmd/%s@latest`. The v1 and v2 plugins share the binary name %q, so reinstalling from the /v2 module switches generation to v2.", connectV2Module, connectLocalPlugin, connectLocalPlugin)
		case kindGotool:
			report.warnAtLinef(filename, line, ruleBufgenGoMod, "the plugin runs via go.mod (%s). Update the tool dependency to the v2 module with `go get -tool %s/cmd/%s` then `go mod tidy`. The buf.gen.yaml entry stays the same.", plugin.ref, connectV2Module, connectLocalPlugin)
		case kindRemote:
			localizeRemotePlugin(filename, plugin, edits, &report)
		}
	}
	if !report.Changed {
		return src, report, nil
	}
	return []byte(strings.Join(edits.apply(), "\n")), report, nil
}

type pluginEntry struct {
	node *yaml.Node
	end  int // exclusive 0-based line index
}

// value also returns the key's index among the keys, not among Content.
func (e pluginEntry) value(key string) (*yaml.Node, int, bool) {
	for index := 0; index+1 < len(e.node.Content); index += 2 {
		if e.node.Content[index].Value == key {
			return e.node.Content[index+1], index / 2, true
		}
	}
	return nil, 0, false
}

func parsePluginEntries(src []byte) ([]pluginEntry, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil
	}
	root := doc.Content[0].Content
	for index := 0; index+1 < len(root); index += 2 {
		if root[index].Value != "plugins" || root[index+1].Kind != yaml.SequenceNode {
			continue
		}
		end := bytes.Count(src, []byte("\n")) + 1
		if index+2 < len(root) {
			end = root[index+2].Line - 1
		}
		items := root[index+1].Content
		entries := make([]pluginEntry, 0, len(items))
		for itemIndex, item := range items {
			if item.Kind != yaml.MappingNode {
				continue
			}
			itemEnd := end
			if itemIndex+1 < len(items) {
				itemEnd = items[itemIndex+1].Line - 1
			}
			entries = append(entries, pluginEntry{node: item, end: itemEnd})
		}
		return entries, nil
	}
	return nil, nil
}

type connectPlugin struct {
	entry    pluginEntry
	keyIndex int // among the keys, not among Content
	kind     string
	ref      string
}

func (p connectPlugin) key() *yaml.Node   { return p.entry.node.Content[2*p.keyIndex] }
func (p connectPlugin) value() *yaml.Node { return p.entry.node.Content[2*p.keyIndex+1] }

// findConnectPlugin accepts the plugin key in any position. A v1 `path` can
// reroute a local plugin through go.mod.
func findConnectPlugin(entry pluginEntry) (connectPlugin, bool) {
	for index := 0; index+1 < len(entry.node.Content); index += 2 {
		key, value := entry.node.Content[index], entry.node.Content[index+1]
		kind, ref, ok := classifyPluginKey(key.Value, value)
		if !ok {
			continue
		}
		plugin := connectPlugin{entry: entry, keyIndex: index / 2, kind: kind, ref: ref}
		if kind == kindLocal {
			if pathValue, _, hasPath := entry.value("path"); hasPath {
				if pathKind, pathRef, ok := classifyLocalPlugin(pathValue); ok {
					plugin.kind, plugin.ref = pathKind, pathRef
				}
			}
		}
		return plugin, true
	}
	return connectPlugin{}, false
}

func classifyPluginKey(key string, value *yaml.Node) (kind, ref string, ok bool) {
	switch key {
	case "local":
		return classifyLocalPlugin(value)
	case "remote":
		if value.Kind == yaml.ScalarNode && isConnectRemoteRef(value.Value) {
			return kindRemote, value.Value, true
		}
	case "name": // v1 syntax
		if value.Kind == yaml.ScalarNode && value.Value == "connect-go" {
			return kindLocal, value.Value, true
		}
	case "plugin": // v1 syntax
		if value.Kind != yaml.ScalarNode {
			return "", "", false
		}
		switch {
		case isConnectRemoteRef(value.Value):
			return kindRemote, value.Value, true
		case value.Value == "connect-go" || value.Value == connectLocalPlugin:
			return kindLocal, value.Value, true
		}
	}
	return "", "", false
}

func classifyLocalPlugin(value *yaml.Node) (kind, ref string, ok bool) {
	if value.Kind == yaml.ScalarNode {
		if value.Value == connectLocalPlugin {
			return kindLocal, value.Value, true
		}
		return "", "", false
	}
	if value.Kind != yaml.SequenceNode || len(value.Content) == 0 {
		return "", "", false
	}
	command := make([]string, 0, len(value.Content))
	for _, element := range value.Content {
		command = append(command, element.Value)
	}
	if path.Base(command[len(command)-1]) != connectLocalPlugin {
		return "", "", false
	}
	ref = "[" + strings.Join(command, ", ") + "]"
	if len(command) >= 2 && command[0] == "go" && (command[1] == "tool" || command[1] == "run") {
		return kindGotool, ref, true
	}
	return kindLocal, ref, true
}

// isConnectRemoteRef reports whether value references either connect-go remote
// plugin, bare or version-tagged.
func isConnectRemoteRef(value string) bool {
	return connectRemotePluginRef.MatchString(value)
}

// localizeRemotePlugin switches to the local plugin because the v2 remote plugin
// is not on the BSR until connect-go v2.0.0 ships.
//
// TODO: pin to the v2 remote plugin once connect-go v2.0.0 is released.
func localizeRemotePlugin(filename string, plugin connectPlugin, edits *lineEdits, report *Report) {
	match := connectRemotePluginRef.FindStringSubmatch(plugin.ref)
	if match == nil {
		return
	}
	if simple, version := match[2] == "gosimple", match[3]; !simple && strings.HasPrefix(version, "v2") {
		return
	}
	line := plugin.key().Line
	// Edit the value before the key, so the key's column stays valid.
	var edited bool
	if plugin.key().Value == "remote" {
		edited = edits.replaceScalar(plugin.value(), connectLocalPlugin) && edits.replaceScalar(plugin.key(), "local")
	} else {
		// v1 `plugin` names a local plugin without the protoc-gen- prefix.
		edited = edits.replaceScalar(plugin.value(), "connect-go")
	}
	if !edited {
		report.warnAtLinef(filename, line, ruleBufgenManualEdit, "switch %s to the local plugin `%s` by hand.", plugin.ref, connectLocalPlugin)
		return
	}
	// `revision` only applies to remote plugins.
	if revision, index, ok := plugin.entry.value("revision"); ok && !edits.deleteKey(plugin.entry, index) {
		report.warnAtLinef(filename, revision.Line, ruleBufgenManualEdit, "remove `revision`, it only applies to remote plugins.")
	}
	report.bump("bufgen_remote_to_local")
	report.warnAtLinef(filename, line, ruleBufgenRemoteUnpublished, "%s has no v2 release yet, so this entry now runs the local plugin. Install it with `go install %s/cmd/%s@latest`, and switch back to the remote plugin once connect-go v2.0.0 is released.", plugin.ref, connectV2Module, connectLocalPlugin)
	// Feeds the regenerate steps, which install the local plugin.
	report.warnAtLinef(filename, line, ruleBufgenReinstall, "install the generator with `go install %s/cmd/%s@latest`.", connectV2Module, connectLocalPlugin)
}

func stripSimpleOpt(filename string, plugin connectPlugin, edits *lineEdits, report *Report) {
	opt, optIndex, ok := plugin.entry.value("opt")
	if !ok {
		return
	}
	var edited bool
	switch opt.Kind {
	case yaml.ScalarNode:
		var kept []string
		for token := range strings.SplitSeq(opt.Value, ",") {
			if !isSimpleToken(token) {
				kept = append(kept, strings.TrimSpace(token))
			}
		}
		if len(kept) == len(strings.Split(opt.Value, ",")) {
			return
		}
		if len(kept) == 0 {
			edited = edits.deleteKey(plugin.entry, optIndex)
		} else {
			edited = edits.replaceScalar(opt, strings.Join(kept, ","))
		}
	case yaml.SequenceNode:
		var simple []*yaml.Node
		for _, item := range opt.Content {
			if isSimpleToken(item.Value) {
				simple = append(simple, item)
			}
		}
		if len(simple) == 0 {
			return
		}
		if len(simple) == len(opt.Content) {
			edited = edits.deleteKey(plugin.entry, optIndex)
		} else {
			edited = edits.deleteSequenceItems(opt, simple)
		}
	case yaml.DocumentNode, yaml.MappingNode, yaml.AliasNode:
		return
	}
	if !edited {
		report.warnAtLinef(filename, opt.Line, ruleBufgenManualEdit, "remove the v1 `simple` option by hand. v2 always generates the simple API and rejects the option.")
		return
	}
	report.bump("bufgen_remove_simple")
}

// isSimpleToken reports whether a single opt token is the v1 `simple` flag,
// with or without a value (simple, simple=true, simple=false).
func isSimpleToken(token string) bool {
	token = strings.TrimSpace(token)
	return token == "simple" || strings.HasPrefix(token, "simple=")
}

// lineEdits applies every edit in one pass, so parsed line numbers stay valid.
type lineEdits struct {
	lines   []string
	replace map[int]string
	delete  map[int]bool
}

func (e *lineEdits) current(index int) string {
	if line, ok := e.replace[index]; ok {
		return line
	}
	return e.lines[index]
}

// replaceScalar reports false if the source doesn't match the node.
func (e *lineEdits) replaceScalar(node *yaml.Node, value string) bool {
	index, column := node.Line-1, node.Column-1
	raw, ok := quoteLike(node, node.Value)
	if !ok || index < 0 || index >= len(e.lines) {
		return false
	}
	line := e.current(index)
	if column < 0 || !strings.HasPrefix(line[min(column, len(line)):], raw) {
		return false
	}
	replacement, _ := quoteLike(node, value)
	e.replace[index] = line[:column] + replacement + line[column+len(raw):]
	return true
}

// quoteLike reports false for styles or values it can't reproduce exactly.
func quoteLike(node *yaml.Node, value string) (string, bool) {
	switch {
	case node.Style == 0:
		return value, true
	case node.Style&yaml.DoubleQuotedStyle != 0 && !strings.ContainsAny(value, `"\`):
		return `"` + value + `"`, true
	case node.Style&yaml.SingleQuotedStyle != 0 && !strings.Contains(value, "'"):
		return "'" + value + "'", true
	}
	return "", false
}

// deleteKey replaces a key on the entry's first line with the next key, so the
// sequence dash stays.
func (e *lineEdits) deleteKey(entry pluginEntry, keyIndex int) bool {
	if entry.node.Style&yaml.FlowStyle != 0 {
		return false
	}
	keys := len(entry.node.Content) / 2
	start := entry.node.Content[2*keyIndex].Line - 1
	end := entry.end
	if keyIndex+1 < keys {
		end = entry.node.Content[2*(keyIndex+1)].Line - 1
	}
	// Keep trailing blank and comment lines with whatever follows.
	for end > start+1 && isBlankOrComment(e.lines[end-1]) {
		end--
	}
	if start != entry.node.Line-1 {
		for index := start; index < end; index++ {
			e.delete[index] = true
		}
		return true
	}
	if keyIndex+1 >= keys {
		return false
	}
	// The next key is at the same column, so its nested value stays valid.
	next := entry.node.Content[2*(keyIndex+1)].Line - 1
	column := entry.node.Content[2*keyIndex].Column - 1
	if indentOf(e.current(next)) != column {
		return false
	}
	e.replace[start] = e.current(start)[:column] + e.current(next)[column:]
	for index := start + 1; index < end; index++ {
		e.delete[index] = true
	}
	e.delete[next] = true
	return true
}

func (e *lineEdits) deleteSequenceItems(sequence *yaml.Node, items []*yaml.Node) bool {
	if sequence.Style&yaml.FlowStyle != 0 {
		return false
	}
	for _, item := range items {
		index := item.Line - 1
		raw, ok := quoteLike(item, item.Value)
		if !ok || strings.TrimPrefix(strings.TrimSpace(e.lines[index]), "- ") != raw {
			return false
		}
	}
	for _, item := range items {
		e.delete[item.Line-1] = true
	}
	return true
}

func (e *lineEdits) apply() []string {
	out := make([]string, 0, len(e.lines))
	for index := range e.lines {
		if e.delete[index] {
			continue
		}
		out = append(out, e.current(index))
	}
	return out
}

func isBlankOrComment(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed == "" || strings.HasPrefix(trimmed, "#")
}

func indentOf(line string) int {
	count := 0
	for count < len(line) && line[count] == ' ' {
		count++
	}
	return count
}
