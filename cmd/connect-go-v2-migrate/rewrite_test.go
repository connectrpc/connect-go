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
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestV2CoreSymbols checks that v2CoreSymbols lists exactly the exported
// identifiers of the v2 connect package, so a symbol added to the package is
// not reported as v1-only.
func TestV2CoreSymbols(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join("..", "..", "*.go"))
	if err != nil {
		t.Fatalf("glob connect package: %v", err)
	}
	fset := token.NewFileSet()
	exported := map[string]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			for _, name := range exportedDeclNames(decl) {
				exported[name] = true
			}
		}
	}
	if len(exported) == 0 {
		t.Fatal("no exported identifiers found in the connect package")
	}
	for name := range exported {
		if !v2CoreSymbols[name] {
			t.Errorf("the connect package exports %s, which v2CoreSymbols is missing", name)
		}
	}
	for name := range v2CoreSymbols {
		if !exported[name] {
			t.Errorf("v2CoreSymbols lists %s, which the connect package does not export", name)
		}
	}
}

// exportedDeclNames returns the exported top-level identifiers decl declares.
func exportedDeclNames(decl ast.Decl) []string {
	var names []string
	switch decl := decl.(type) {
	case *ast.FuncDecl:
		if decl.Recv == nil && decl.Name.IsExported() {
			names = append(names, decl.Name.Name)
		}
	case *ast.GenDecl:
		for _, spec := range decl.Specs {
			switch spec := spec.(type) {
			case *ast.TypeSpec:
				if spec.Name.IsExported() {
					names = append(names, spec.Name.Name)
				}
			case *ast.ValueSpec:
				for _, name := range spec.Names {
					if name.IsExported() {
						names = append(names, name.Name)
					}
				}
			}
		}
	}
	return names
}
