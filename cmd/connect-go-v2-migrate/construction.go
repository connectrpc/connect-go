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
	"go/ast"
	"go/printer"
	"go/token"
	"path"
	"strings"
)

// rewriteServerConstruction rewrites the inline
// mux.Handle(<pkg>connect.New<Svc>Handler(svc, opts...)) shape to the v2
// server/register/mount form. Consecutive matches on the same mux with
// identical options share one *connect.Server (v2 carries interceptors per
// server). Candidate blocks are collected before mutation so a node is never
// rewritten mid-traversal.
func rewriteServerConstruction(file *ast.File, state *rewriteState, report *Report) {
	ictx := newEcosystemContext(file)
	stubs := connectStubAliases(file)
	interceptorVars := interceptorOptionVars(file, state.connectAlias)
	var blocks []*ast.BlockStmt
	walk(file, func(n ast.Node) {
		if block, ok := n.(*ast.BlockStmt); ok {
			blocks = append(blocks, block)
		}
	})
	for _, block := range blocks {
		// One server per (mux, options) group; a second group on the same mux is
		// flagged, since v2 applies interceptors per server.
		seenMux := map[string]string{}
		for index := 0; index < len(block.List); index++ {
			match, ok := matchConstruction(block.List[index], state, ictx, interceptorVars, stubs)
			if !ok {
				continue
			}
			muxKey := exprString(match.mux)
			if prior, ok := seenMux[muxKey]; ok && prior != match.groupKey {
				report.warnAtf(block.List[index].Pos(), ruleHandlerConstruction, "handlers on %s have differing options, so each group gets its own *connect.Server. v2 interceptors apply per server.", muxKey)
			} else if !ok {
				seenMux[muxKey] = match.groupKey
			}
			// Extend the run with consecutive matches that can share the server.
			run := []constructionMatch{match}
			for next := index + 1; next < len(block.List); next++ {
				nextMatch, ok := matchConstruction(block.List[next], state, ictx, interceptorVars, stubs)
				if !ok || nextMatch.groupKey != match.groupKey {
					break
				}
				run = append(run, nextMatch)
			}
			repl := buildConstructionReplacement(block, run, state, report, ictx)
			block.List = append(block.List[:index:index], append(repl, block.List[index+len(run):]...)...)
			index += len(repl) - 1
		}
	}
}

// constructionMatch holds the parsed parts of one
// `<mux>.Handle(<ctor>(svc, opts...))` statement. groupKey is the printed
// (mux, options) pair; statements with equal keys can share a server.
type constructionMatch struct {
	mux          ast.Expr
	pkgIdent     *ast.Ident
	registerName string
	counter      string
	svc          ast.Expr
	interceptors []ast.Expr
	otherOpts    []ast.Expr
	groupKey     string
}

// matchConstruction matches `<mux>.Handle(<pkg>connect.New<Svc>Handler(svc, opts...))`
// (or grpchealth.NewHandler) and returns its parsed parts without mutating it.
func matchConstruction(stmt ast.Stmt, state *rewriteState, ictx ecosystemContext, interceptorVars, stubs map[string]bool) (constructionMatch, bool) {
	exprStmt, isExprStmt := stmt.(*ast.ExprStmt)
	if !isExprStmt {
		return constructionMatch{}, false
	}
	handleCall, isCall := exprStmt.X.(*ast.CallExpr)
	if !isCall || len(handleCall.Args) != 1 {
		return constructionMatch{}, false
	}
	handleSel, isSel := handleCall.Fun.(*ast.SelectorExpr)
	if !isSel || handleSel.Sel.Name != "Handle" {
		return constructionMatch{}, false
	}
	ctorCall, isCtorCall := handleCall.Args[0].(*ast.CallExpr)
	if !isCtorCall || len(ctorCall.Args) == 0 {
		return constructionMatch{}, false
	}
	ctorSel, isCtorSel := ctorCall.Fun.(*ast.SelectorExpr)
	if !isCtorSel {
		return constructionMatch{}, false
	}
	pkgIdent, isIdent := ctorSel.X.(*ast.Ident)
	if !isIdent {
		return constructionMatch{}, false
	}
	// grpchealth.NewHandler -> grpchealth.Register; generated New<Svc>Handler
	// -> Register<Svc>Handler. grpchealth matches first because a user alias may
	// also end in "connect".
	var registerName, counter string
	switch {
	case ictx.healthAlias != "" && pkgIdent.Name == ictx.healthAlias && ctorSel.Sel.Name == "NewHandler":
		registerName, counter = "Register", "grpchealth_register"
	case stubs[pkgIdent.Name]:
		generatedName, isHandlerCtor := registerHandlerName(ctorSel.Sel.Name)
		if !isHandlerCtor {
			return constructionMatch{}, false
		}
		registerName, counter = generatedName, "server_construction"
	default:
		return constructionMatch{}, false
	}

	interceptors, otherOpts := splitHandlerOptions(ctorCall.Args[1:], state, interceptorVars)
	var key strings.Builder
	key.WriteString(exprString(handleSel.X))
	for _, opt := range ctorCall.Args[1:] {
		key.WriteString("|")
		key.WriteString(exprString(opt))
	}
	return constructionMatch{
		mux:          handleSel.X,
		pkgIdent:     pkgIdent,
		registerName: registerName,
		counter:      counter,
		svc:          ctorCall.Args[0],
		interceptors: interceptors,
		otherOpts:    otherOpts,
		groupKey:     key.String(),
	}, true
}

// buildConstructionReplacement emits the v2 statements for a run of matches
// sharing one server: a connect.NewServer assignment, one Register call per
// match, and a single connecthttp.Mount.
func buildConstructionReplacement(block *ast.BlockStmt, run []constructionMatch, state *rewriteState, report *Report, ictx ecosystemContext) []ast.Stmt {
	first := run[0]
	mapped := make([]ast.Expr, 0, len(first.interceptors))
	for _, interceptor := range first.interceptors {
		mapped = append(mapped, mapInterceptor(interceptor, report, ictx, "Server"))
	}

	serverName := uniqueIdent(block, "server", "srv")
	stmts := make([]ast.Stmt, 0, len(run)+2)
	stmts = append(stmts, &ast.AssignStmt{
		Lhs: []ast.Expr{ast.NewIdent(serverName)},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{callExpr(state.connectV2Alias, "NewServer", mapped...)},
	})
	for _, match := range run {
		stmts = append(stmts, &ast.ExprStmt{X: &ast.CallExpr{
			Fun:  &ast.SelectorExpr{X: match.pkgIdent, Sel: ast.NewIdent(match.registerName)},
			Args: []ast.Expr{ast.NewIdent(serverName), match.svc},
		}})
		report.bump(match.counter)
	}
	mountArgs := append([]ast.Expr{first.mux, ast.NewIdent(serverName)}, first.otherOpts...)
	stmts = append(stmts, &ast.ExprStmt{X: callExpr(state.connectHTTPAlias, "Mount", mountArgs...)})

	state.usedV2 = true
	state.usedConnectHTTP = true
	return stmts
}

// splitHandlerOptions partitions constructor options into interceptors (inline
// connect.WithInterceptors args, or a variable named in interceptorVars that
// holds such a value) and the rest. Interceptors move to
// connect.NewServer/NewClient, the rest to connecthttp.Mount/NewTransport.
func splitHandlerOptions(opts []ast.Expr, state *rewriteState, interceptorVars map[string]bool) (interceptors, other []ast.Expr) {
	for _, opt := range opts {
		if call, ok := opt.(*ast.CallExpr); ok && isConnectSelector(call.Fun, state.connectAlias, "WithInterceptors") {
			interceptors = append(interceptors, call.Args...)
			continue
		}
		if id, ok := opt.(*ast.Ident); ok && interceptorVars[id.Name] {
			interceptors = append(interceptors, opt)
			continue
		}
		other = append(other, opt)
	}
	return interceptors, other
}

// interceptorOptionVars returns the names of local variables assigned a
// connect.WithInterceptors(...) value.
func interceptorOptionVars(file *ast.File, connectAlias string) map[string]bool {
	vars := map[string]bool{}
	walk(file, func(n ast.Node) {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != len(assign.Rhs) {
			return
		}
		for i, rhs := range assign.Rhs {
			call, isCall := rhs.(*ast.CallExpr)
			if !isCall || !isConnectSelector(call.Fun, connectAlias, "WithInterceptors") {
				continue
			}
			if id, isIdent := assign.Lhs[i].(*ast.Ident); isIdent && id.Name != "_" {
				vars[id.Name] = true
			}
		}
	})
	return vars
}

// mapInterceptor rewrites a known v1 interceptor constructor to its v2
// side-specific form (validate.NewInterceptor -> validate.New<side>Interceptor).
// side is "Server" or "Client". Unknown or error-returning interceptors
// (custom ones, and otelconnect) are left untouched and flagged.
func mapInterceptor(interceptor ast.Expr, report *Report, ictx ecosystemContext, side string) ast.Expr {
	warnCustom := func() ast.Expr {
		report.warnAtf(interceptor.Pos(), ruleInterceptorMigration, "interceptor %s stays in the connect.New%s(...) list unmigrated. Its v2 type is connect.%sInterceptor.", exprString(interceptor), side, side)
		return interceptor
	}
	call, isCall := interceptor.(*ast.CallExpr)
	if !isCall {
		return warnCustom()
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return warnCustom()
	}
	pkg, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return warnCustom()
	}
	switch {
	case pkg.Name == ictx.validateAlias && sel.Sel.Name == newInterceptorName:
		sel.Sel.Name = "New" + side + "Interceptor"
		report.bump("interceptor_validate_v2")
		return call
	case pkg.Name == ictx.otelAlias && sel.Sel.Name == newInterceptorName:
		report.warnAtf(interceptor.Pos(), ruleInterceptorMigration, "otelconnect.NewInterceptor stays in the connect.New%s(...) list unmigrated. v2 uses otelconnect.New%sInterceptor (connectrpc.com/otelconnect/v2), which returns an error and is assigned before the constructor.", side, side)
		return call
	default:
		return warnCustom()
	}
}

// rewriteClientConstruction rewrites the v1 generated client constructor
// <pkg>connect.New<Svc>Client(httpClient, baseURL, opts...) to the v2 form that
// takes a *connect.Client built from a transport. Interceptor options move to
// connect.NewClient, the rest to connecthttp.NewTransport.
func rewriteClientConstruction(file *ast.File, state *rewriteState, report *Report) {
	ictx := newEcosystemContext(file)
	stubs := connectStubAliases(file)
	interceptorVars := interceptorOptionVars(file, state.connectAlias)
	walk(file, func(n ast.Node) {
		call, isCall := n.(*ast.CallExpr)
		if !isCall || len(call.Args) < 2 {
			return
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			return
		}
		// grpcreflect.NewClient shares the v1 (httpClient, baseURL, opts...) shape.
		counter := "client_construction"
		if isReflectClientSelector(sel, ictx) {
			counter = "grpcreflect_client"
		} else if !isClientConstructorSelector(sel, stubs) {
			return
		}
		interceptors, otherOpts := splitHandlerOptions(call.Args[2:], state, interceptorVars)
		mapped := make([]ast.Expr, 0, len(interceptors))
		for _, interceptor := range interceptors {
			mapped = append(mapped, mapInterceptor(interceptor, report, ictx, "Client"))
		}
		transportArgs := append([]ast.Expr{call.Args[0], call.Args[1]}, otherOpts...)
		transport := callExpr(state.connectHTTPAlias, "NewTransport", transportArgs...)
		newClient := callExpr(state.connectV2Alias, "NewClient", append([]ast.Expr{transport}, mapped...)...)
		call.Args = []ast.Expr{newClient}
		state.usedV2 = true
		state.usedConnectHTTP = true
		report.bump(counter)
	})
}

func isReflectClientSelector(sel *ast.SelectorExpr, ictx ecosystemContext) bool {
	pkg, ok := sel.X.(*ast.Ident)
	return ok && ictx.reflectAlias != "" && pkg.Name == ictx.reflectAlias && sel.Sel.Name == "NewClient"
}

// connectStubAliases returns the local names bound to imported generated connect
// stub packages (path's last element ends in "connect"). Matching against this
// set avoids firing on an unrelated identifier or a dot/blank import.
func connectStubAliases(file *ast.File) map[string]bool {
	aliases := map[string]bool{}
	for _, imp := range file.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)
		if !strings.HasSuffix(path.Base(importPath), "connect") {
			continue
		}
		name := path.Base(importPath)
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				continue
			}
			name = imp.Name.Name
		}
		aliases[name] = true
	}
	return aliases
}

// isClientConstructorSelector reports whether sel is a generated
// <pkg>connect.New<Svc>Client constructor.
func isClientConstructorSelector(sel *ast.SelectorExpr, stubs map[string]bool) bool {
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || !stubs[pkg.Name] {
		return false
	}
	name := sel.Sel.Name
	return strings.HasPrefix(name, "New") && strings.HasSuffix(name, "Client") && len(name) > len("NewClient")
}

// isStubConstructorSelector reports whether sel is a generated connect
// constructor: <pkg>connect.New<Name>Client or <pkg>connect.New<Name>Handler.
func isStubConstructorSelector(sel *ast.SelectorExpr, stubs map[string]bool) bool {
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || !stubs[pkg.Name] {
		return false
	}
	name := sel.Sel.Name
	if !strings.HasPrefix(name, "New") {
		return false
	}
	return (strings.HasSuffix(name, "Client") && len(name) > len("NewClient")) ||
		(strings.HasSuffix(name, "Handler") && len(name) > len("NewHandler"))
}

// usesConnectStub reports whether the file calls any generated connect
// constructor, so files that touch connect only through stubs are still processed.
func usesConnectStub(file *ast.File) bool {
	stubs := connectStubAliases(file)
	found := false
	walk(file, func(n ast.Node) {
		if found {
			return
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isStubConstructorSelector(sel, stubs) {
				found = true
			}
		}
	})
	return found
}

// firstStubDependentPos returns the position of the first pattern whose rewrite
// depends on regenerated v2 bindings (generated constructors, Request/Response
// wrappers and constructors, or stream types), and whether one was found.
func firstStubDependentPos(file *ast.File, connectAlias string) (token.Pos, bool) {
	stubs := connectStubAliases(file)
	var pos token.Pos
	walk(file, func(n ast.Node) {
		if pos.IsValid() {
			return
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return
		}
		if isStubConstructorSelector(sel, stubs) {
			pos = sel.Pos()
			return
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == connectAlias {
			switch sel.Sel.Name {
			case "Request", "Response", "NewRequest", "NewResponse",
				"ClientStream", "ServerStream", "BidiStream":
				pos = sel.Pos()
			}
		}
	})
	return pos, pos.IsValid()
}

// registerHandlerName maps New<Svc>Handler to Register<Svc>Handler, returning
// false for names that don't fit that shape.
func registerHandlerName(ctor string) (string, bool) {
	if !strings.HasPrefix(ctor, "New") || !strings.HasSuffix(ctor, "Handler") || len(ctor) <= len("NewHandler") {
		return "", false
	}
	return "Register" + strings.TrimPrefix(ctor, "New"), true
}

// newInterceptorName is the v1 constructor (validate, otelconnect) that v2
// splits into server and client forms.
const newInterceptorName = "NewInterceptor"

// ecosystemContext holds the local import names for the recognised ecosystem
// packages (empty when the file does not import one).
type ecosystemContext struct {
	validateAlias     string
	otelAlias         string
	authnAlias        string
	healthAlias       string
	reflectAlias      string
	vanguardAlias     string
	vanguardGRPCAlias string
}

func newEcosystemContext(file *ast.File) ecosystemContext {
	return ecosystemContext{
		validateAlias:     importLocalName(file, "connectrpc.com/validate"),
		otelAlias:         importLocalName(file, "connectrpc.com/otelconnect"),
		authnAlias:        importLocalName(file, "connectrpc.com/authn"),
		healthAlias:       importLocalName(file, "connectrpc.com/grpchealth"),
		reflectAlias:      importLocalName(file, "connectrpc.com/grpcreflect"),
		vanguardAlias:     importLocalName(file, "connectrpc.com/vanguard"),
		vanguardGRPCAlias: importLocalName(file, "connectrpc.com/vanguard/vanguardgrpc"),
	}
}

// importLocalName returns the local name a file uses for importPath (explicit
// alias, else the path's last element), or "" if it isn't imported.
func importLocalName(file *ast.File, importPath string) string {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != importPath {
			continue
		}
		if imp.Name != nil {
			// Dot/blank imports bind no usable qualifier.
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				return ""
			}
			return imp.Name.Name
		}
		return path.Base(importPath)
	}
	return ""
}

func callExpr(pkg, fn string, args ...ast.Expr) *ast.CallExpr {
	return &ast.CallExpr{
		Fun:  &ast.SelectorExpr{X: ast.NewIdent(pkg), Sel: ast.NewIdent(fn)},
		Args: args,
	}
}

func exprString(expr ast.Expr) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, token.NewFileSet(), expr); err != nil {
		return "<expr>"
	}
	return buf.String()
}
