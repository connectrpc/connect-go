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
	"slices"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// TODO: switch to tagged releases once the ecosystem v2 modules are tagged.
const ecosystemVersionQuery = "@main"

// ecosystemImports keep their paths, so only their call sites change. authn is
// left out because its API is unchanged.
var ecosystemImports = [...]string{
	"connectrpc.com/validate",
	"connectrpc.com/otelconnect",
}

var movedEcosystemImports = [...][2]string{
	{"connectrpc.com/grpchealth", "connectrpc.com/grpchealth/v2"},
	{"connectrpc.com/grpcreflect", "connectrpc.com/grpcreflect/v2"},
}

// manualEcosystemImports are ecosystem packages whose API changes too much to
// rewrite, so the tool only warns.
var manualEcosystemImports = [...]string{
	"connectrpc.com/vanguard",
	"connectrpc.com/vanguard/vanguardgrpc",
}

func isEcosystemImport(importPath string) bool {
	_, moved := movedEcosystemTarget(importPath)
	return moved || slices.Contains(ecosystemImports[:], importPath)
}

func movedEcosystemTarget(importPath string) (string, bool) {
	for _, mod := range movedEcosystemImports {
		if importPath == mod[0] {
			return mod[1], true
		}
	}
	return "", false
}

func isMovedEcosystemModule(path string) bool {
	for _, mod := range movedEcosystemImports {
		if path == mod[1] {
			return true
		}
	}
	return false
}

// hasEcosystemImport reports whether the file imports any v1 ecosystem package,
// so files touching connect only through one are still processed.
func hasEcosystemImport(file *ast.File) bool {
	for _, imp := range file.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)
		if isEcosystemImport(importPath) || slices.Contains(manualEcosystemImports[:], importPath) {
			return true
		}
	}
	return false
}

// firstEcosystemImportPos returns the position of the first migrated import.
func firstEcosystemImportPos(file *ast.File) token.Pos {
	for _, imp := range file.Imports {
		if isEcosystemImport(strings.Trim(imp.Path.Value, `"`)) {
			return imp.Pos()
		}
	}
	return token.NoPos
}

func rewriteEcosystemImports(fset *token.FileSet, file *ast.File, state *rewriteState, report *Report) {
	if !state.stubsReady {
		return
	}
	for _, mod := range movedEcosystemImports {
		if astutil.RewriteImport(fset, file, mod[0], mod[1]) {
			report.bump("import_ecosystem_v2")
		}
	}
}

// warnEcosystemCalls flags ecosystem call sites whose v2 form the tool can't
// produce mechanically, naming the replacement.
func warnEcosystemCalls(file *ast.File, report *Report) {
	ectx := newEcosystemContext(file)
	for _, imp := range file.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)
		if slices.Contains(manualEcosystemImports[:], importPath) {
			report.warnAtf(imp.Pos(), ruleEcosystemMigration, "%s is not migrated automatically. Its v2 API changes substantially and needs new configuration: services register on a connect.Server and REST routes mount with vanguard.Mount. See docs/v2-migration.md", importPath)
		}
	}
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
		case pkg.Name == ectx.reflectAlias && name == "WithRequestHeaders":
			report.warnAtf(call.Pos(), ruleEcosystemMigration, "grpcreflect.WithRequestHeaders is removed in v2. Set headers on the context passed to NewStream with connect.NewClientContext(ctx) and info.RequestHeader().")
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

// rewriteReflectStreams handles the v2 ClientStream, where Close returns only an
// error and Spec, Peer and ResponseHeader are removed.
func rewriteReflectStreams(file *ast.File, report *Report) {
	if importLocalName(file, "connectrpc.com/grpcreflect") == "" {
		return
	}
	streams := reflectStreamVars(file)
	if len(streams) == 0 {
		return
	}
	walk(file, func(n ast.Node) {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) != 2 || len(node.Rhs) != 1 || !isStreamMethodCall(node.Rhs[0], streams, "Close") {
				return
			}
			if blank, ok := node.Lhs[0].(*ast.Ident); ok && blank.Name == "_" {
				node.Lhs = node.Lhs[1:]
				report.bump("grpcreflect_stream_close")
				return
			}
			report.warnAtf(node.Pos(), ruleEcosystemMigration, "grpcreflect ClientStream.Close returns only an error in v2. Read response headers from the connect.NewClientContext(ctx) info passed to NewStream.")
		case *ast.CallExpr:
			for _, name := range [...]string{"Spec", "Peer", "ResponseHeader"} {
				if isStreamMethodCall(node, streams, name) {
					report.warnAtf(node.Pos(), ruleEcosystemMigration, "grpcreflect ClientStream.%s is removed in v2. Read call metadata from the connect.NewClientContext(ctx) info passed to NewStream.", name)
				}
			}
		}
	})
}

// reflectStreamVars matches by name because the per-file rewrite has no type
// info.
func reflectStreamVars(file *ast.File) map[string]bool {
	streams := map[string]bool{}
	walk(file, func(n ast.Node) {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return
		}
		call, isCall := assign.Rhs[0].(*ast.CallExpr)
		if !isCall {
			return
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		ident, isIdent := assign.Lhs[0].(*ast.Ident)
		if isSel && isIdent && sel.Sel.Name == "NewStream" {
			streams[ident.Name] = true
		}
	})
	return streams
}

func isStreamMethodCall(expr ast.Expr, streams map[string]bool, method string) bool {
	call, isCall := expr.(*ast.CallExpr)
	if !isCall {
		return false
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != method {
		return false
	}
	recv, isIdent := sel.X.(*ast.Ident)
	return isIdent && streams[recv.Name]
}
