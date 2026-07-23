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
	"testing"
)

func parseFileForTest(t *testing.T, src string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return file
}

// TestConnectStubAliases checks which imports are treated as connect stub
// packages: a normal or aliased import whose path ends in "connect" qualifies,
// while dot and blank imports (which bind no usable qualifier) and unrelated
// packages do not.
func TestConnectStubAliases(t *testing.T) {
	t.Parallel()
	src := `package app

import (
	"example.com/gen/ping/v1/pingv1connect"
	pc "example.com/gen/pong/v1/pongv1connect"
	. "example.com/gen/dot/v1/dotv1connect"
	_ "example.com/gen/blank/v1/blankv1connect"
	"fmt"
)
`
	aliases := connectStubAliases(parseFileForTest(t, src))
	for name, want := range map[string]bool{
		"pingv1connect":  true,  // default name
		"pc":             true,  // explicit alias
		"dotv1connect":   false, // dot import: symbols are unqualified
		"blankv1connect": false, // blank import: never referenced
		"fmt":            false, // unrelated
	} {
		if got := aliases[name]; got != want {
			t.Errorf("connectStubAliases[%q] = %v, want %v", name, got, want)
		}
	}
}

// TestStubConstructorSelectorGating checks that a generated-constructor shape is
// matched only when its package qualifier is an imported connect stub. A
// same-named local value (e.g. a variable called "myconnect") must not be
// mistaken for a stub package.
func TestStubConstructorSelectorGating(t *testing.T) {
	t.Parallel()
	newSel := func(pkg, name string) *ast.SelectorExpr {
		return &ast.SelectorExpr{X: ast.NewIdent(pkg), Sel: ast.NewIdent(name)}
	}
	stubs := map[string]bool{"pingv1connect": true}
	tests := []struct {
		name     string
		sel      *ast.SelectorExpr
		wantStub bool
		wantClnt bool
	}{
		{name: "imported stub client", sel: newSel("pingv1connect", "NewPingServiceClient"), wantStub: true, wantClnt: true},
		{name: "imported stub handler", sel: newSel("pingv1connect", "NewPingServiceHandler"), wantStub: true, wantClnt: false},
		{name: "local var ending in connect", sel: newSel("myconnect", "NewPingServiceClient"), wantStub: false, wantClnt: false},
		{name: "bare NewClient", sel: newSel("pingv1connect", "NewClient"), wantStub: false, wantClnt: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isStubConstructorSelector(test.sel, stubs); got != test.wantStub {
				t.Errorf("isStubConstructorSelector = %v, want %v", got, test.wantStub)
			}
			if got := isClientConstructorSelector(test.sel, stubs); got != test.wantClnt {
				t.Errorf("isClientConstructorSelector = %v, want %v", got, test.wantClnt)
			}
		})
	}
}

// TestImportLocalName checks the local name resolved for an import, including
// dot and blank imports, which bind no qualifier and so report "".
func TestImportLocalName(t *testing.T) {
	t.Parallel()
	src := `package app

import (
	"connectrpc.com/grpchealth"
	v "connectrpc.com/validate"
	. "connectrpc.com/grpcreflect"
	_ "connectrpc.com/otelconnect"
)
`
	file := parseFileForTest(t, src)
	tests := []struct {
		path string
		want string
	}{
		{path: "connectrpc.com/grpchealth", want: "grpchealth"},
		{path: "connectrpc.com/validate", want: "v"},
		{path: "connectrpc.com/grpcreflect", want: ""}, // dot import
		{path: "connectrpc.com/otelconnect", want: ""}, // blank import
		{path: "connectrpc.com/missing", want: ""},     // not imported
	}
	for _, test := range tests {
		if got := importLocalName(file, test.path); got != test.want {
			t.Errorf("importLocalName(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}
