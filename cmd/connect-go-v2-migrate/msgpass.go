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
	"go/ast"
	"go/types"
	"os"
	"sort"

	"golang.org/x/tools/go/packages"
)

// msgEdit holds one file's dangling-.Msg result: the content offsets index
// into, the content after stripping, and the number of strips.
type msgEdit struct {
	base    []byte
	content []byte
	count   int
}

// stripDanglingMsg removes `.Msg` selectors left dangling after a return-type
// flip to the bare message *T (e.g. a caller's `got.Msg` in another file,
// including _test.go, that the per-file pass never sees).
//
// It is a type-directed post-pass: reload the project with the per-file rewrites
// as an in-memory overlay (and Tests:true), then delete every X.Msg whose
// receiver type no longer has a Msg field or method. Edits are byte splices at
// token offsets, so formatting and comments are preserved. overlay and the
// returned map are keyed by absolute path; the result holds only changed files.
func stripDanglingMsg(roots []string, overlay map[string][]byte) (map[string]msgEdit, error) {
	base := packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo,
		Tests:   true,
		Overlay: overlay,
	}
	pkgs, err := loadGoPackages(roots, base)
	if err != nil {
		return nil, err
	}

	// Collect byte ranges to delete, grouped by file. A file can appear in
	// several package variants (normal and test); take the union of their ranges,
	// since each strip is only emitted for a type that definitely lacks Msg.
	deletions := map[string]map[offsetRange]bool{}
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			path := pkg.Fset.Position(file.Pos()).Filename
			if isGeneratedAST(file) {
				continue
			}
			ranges := danglingMsgRanges(file, pkg)
			if len(ranges) == 0 {
				continue
			}
			set := deletions[path]
			if set == nil {
				set = map[offsetRange]bool{}
				deletions[path] = set
			}
			for _, r := range ranges {
				set[r] = true
			}
		}
	}

	edits := map[string]msgEdit{}
	for path, set := range deletions {
		if len(set) == 0 {
			continue
		}
		ranges := make([]offsetRange, 0, len(set))
		for r := range set {
			ranges = append(ranges, r)
		}
		content, ok := overlay[path]
		if !ok {
			disk, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil, readErr
			}
			content = disk
		}
		edits[path] = msgEdit{base: content, content: applyDeletions(content, ranges), count: len(ranges)}
	}
	return edits, nil
}

// offsetRange is a half-open byte range [start, end) to delete from a file.
type offsetRange struct {
	start, end int
}

// danglingMsgRanges returns the byte ranges of `.Msg` selectors in file whose
// receiver type no longer has a Msg field or method.
func danglingMsgRanges(file *ast.File, pkg *packages.Package) []offsetRange {
	// Skip `.Msg` that is the function being called (stream.Msg()): stripping it
	// would leave dangling parens. Only field-access .Msg is stripped.
	calledMsg := map[*ast.SelectorExpr]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == identMsg {
				calledMsg[sel] = true
			}
		}
		return true
	})
	var ranges []offsetRange
	ast.Inspect(file, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != identMsg || calledMsg[sel] {
			return true
		}
		recv := pkg.TypesInfo.TypeOf(sel.X)
		if recv == nil || !typeMissingMsg(recv, pkg.Types) {
			return true
		}
		start := pkg.Fset.Position(sel.X.End()).Offset
		end := pkg.Fset.Position(sel.Sel.End()).Offset
		ranges = append(ranges, offsetRange{start: start, end: end})
		return true
	})
	return ranges
}

// typeMissingMsg reports whether typ has neither a field nor a method named Msg.
// A nil or invalid type returns false, so the selector is left alone without
// confident type information.
func typeMissingMsg(typ types.Type, pkg *types.Package) bool {
	if typ == nil || typ == types.Typ[types.Invalid] {
		return false
	}
	if basic, ok := typ.Underlying().(*types.Basic); ok && basic.Kind() == types.Invalid {
		return false
	}
	obj, _, _ := types.LookupFieldOrMethod(typ, true, pkg, identMsg)
	return obj == nil
}

// applyDeletions removes the given byte ranges from content, back to front so
// earlier offsets stay valid.
func applyDeletions(content []byte, ranges []offsetRange) []byte {
	sorted := append([]offsetRange(nil), ranges...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start > sorted[j].start })
	out := append([]byte(nil), content...)
	for _, r := range sorted {
		if r.start < 0 || r.end > len(out) || r.start > r.end {
			continue
		}
		out = append(out[:r.start], out[r.end:]...)
	}
	return out
}
