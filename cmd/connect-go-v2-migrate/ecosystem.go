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
	"go/token"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// ecosystemImports maps v1 ecosystem import paths to their /v2 paths.
// Subpackages need their own entry (rewrites match the exact path).
var ecosystemImports = [...][2]string{
	{"connectrpc.com/validate", "connectrpc.com/validate/v2"},
	{"connectrpc.com/otelconnect", "connectrpc.com/otelconnect/v2"},
	{"connectrpc.com/authn", "connectrpc.com/authn/v2"},
	{"connectrpc.com/grpchealth", "connectrpc.com/grpchealth/v2"},
	{"connectrpc.com/grpcreflect", "connectrpc.com/grpcreflect/v2"},
	{"connectrpc.com/vanguard", "connectrpc.com/vanguard/v2"},
	{"connectrpc.com/vanguard/vanguardgrpc", "connectrpc.com/vanguard/v2/vanguardgrpc"},
}

// ecosystemV2Module returns the v2 module path to `go get` for a v1 ecosystem
// import, or "" if it's not one. It truncates at "/v2" so a subpackage import
// resolves to its module root (vanguard/vanguardgrpc -> vanguard/v2).
func ecosystemV2Module(importPath string) string {
	for _, mod := range ecosystemImports {
		if importPath != mod[0] {
			continue
		}
		v2 := mod[1]
		if index := strings.Index(v2, "/v2"); index >= 0 {
			return v2[:index+len("/v2")]
		}
		return v2
	}
	return ""
}

// hasEcosystemImport reports whether the file imports any v1 ecosystem package,
// so files touching connect only through one are still processed.
func hasEcosystemImport(file *ast.File) bool {
	for _, imp := range file.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)
		for _, mod := range ecosystemImports {
			if importPath == mod[0] {
				return true
			}
		}
	}
	return false
}

// firstEcosystemImportPos returns the position of the first v1 ecosystem import.
func firstEcosystemImportPos(file *ast.File) token.Pos {
	for _, imp := range file.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)
		for _, mod := range ecosystemImports {
			if importPath == mod[0] {
				return imp.Pos()
			}
		}
	}
	return token.NoPos
}

// rewriteEcosystemImports flips every v1 ecosystem import to its /v2 module,
// once the generated bindings are v2.
func rewriteEcosystemImports(fset *token.FileSet, file *ast.File, state *rewriteState, report *Report) {
	if !state.stubsReady {
		return
	}
	for _, mod := range ecosystemImports {
		if astutil.RewriteImport(fset, file, mod[0], mod[1]) {
			report.bump("import_ecosystem_v2")
		}
	}
}

// warnEcosystemCalls flags ecosystem call sites whose v2 form the tool can't
// produce mechanically, naming the replacement.
func warnEcosystemCalls(file *ast.File, report *Report) {
	ectx := newEcosystemContext(file)
	walk(file, func(n ast.Node) {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			return
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		if !isIdent || pkg.Name == "" {
			return
		}
		// The construction pass already warns interceptor options it relocated
		// (e.g. otelconnect.NewInterceptor moved into connect.NewServer); don't
		// add a second diagnostic at the same call site.
		if report.warnedAt(call.Pos()) {
			return
		}
		name := sel.Sel.Name
		switch {
		case pkg.Name == ectx.authnAlias && name == "NewMiddleware":
			report.warnAtf(call.Pos(), ruleEcosystemMigration, "authn.NewMiddleware -> authn.NewServerInterceptor(authFunc) passed to connect.NewServer. AuthFunc takes (ctx, connect.Spec, *connect.Header). See docs/v2-migration.md")
		case pkg.Name == ectx.reflectAlias && (name == "NewHandlerV1" || name == "NewHandlerV1Alpha" || name == "NewStaticReflector" || name == "NewReflector"):
			report.warnAtf(call.Pos(), ruleEcosystemMigration, "grpcreflect.%s -> grpcreflect.Register(server) serves v1 and v1alpha and lists the server's registered services by default. See docs/v2-migration.md", name)
		case pkg.Name == ectx.vanguardAlias && (name == "NewTranscoder" || name == "NewService" || name == "NewServiceWithSchema"):
			report.warnAtf(call.Pos(), ruleEcosystemMigration, "vanguard.%s -> vanguard.Mount(mux, server) mounts REST routes for registered methods with google.api.http annotations. See docs/v2-migration.md", name)
		case pkg.Name == ectx.vanguardGRPCAlias && name == "NewTranscoder":
			report.warnAtf(call.Pos(), ruleEcosystemMigration, "vanguardgrpc.NewTranscoder -> vanguardgrpc.NewServiceRegistrar(server). See docs/v2-migration.md")
		case pkg.Name == ectx.otelAlias && name == newInterceptorName:
			report.warnAtf(call.Pos(), ruleEcosystemMigration, "otelconnect.NewInterceptor -> otelconnect.NewServerInterceptor or otelconnect.NewClientInterceptor, depending on use. Both return an error.")
		case pkg.Name == ectx.validateAlias && name == newInterceptorName:
			report.warnAtf(call.Pos(), ruleEcosystemMigration, "validate.NewInterceptor -> validate.NewServerInterceptor or validate.NewClientInterceptor, depending on use")
		}
	})
}
