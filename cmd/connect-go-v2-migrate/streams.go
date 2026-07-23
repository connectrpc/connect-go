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
	"sort"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// handlerStreamType is a resolved v2 generated handler stream type.
type handlerStreamType struct {
	pkgPath  string
	pkgName  string
	name     string   // <Service><RPC>ServerStream
	messages []string // sorted base names of the request/response messages
}

// handlerStreamResolver resolves a handler's RPC name to its v2 stream type,
// matching on both the name and the message types because the name alone is
// ambiguous ("Sum" is a suffix of CumSumServerStream).
type handlerStreamResolver struct {
	types []handlerStreamType
}

// lookup returns the v2 handler stream type for an RPC name and its message
// types. recvType disambiguates when several services share an RPC name and
// messages; unresolved matches are returned as ambiguous for the caller's warning.
func (r *handlerStreamResolver) lookup(method, recvType string, messages []string) (match handlerStreamType, ambiguous []handlerStreamType, ok bool) {
	if r == nil || method == "" || len(messages) == 0 {
		return handlerStreamType{}, nil, false
	}
	suffix := method + "ServerStream"
	want := sortedStrings(messages)
	var matches []handlerStreamType
	for _, candidate := range r.types {
		if strings.HasSuffix(candidate.name, suffix) && equalStrings(candidate.messages, want) {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 0:
		return handlerStreamType{}, nil, false
	case 1:
		return matches[0], nil, true
	}
	if picked, ok := disambiguateByReceiver(matches, suffix, recvType); ok {
		return picked, nil, true
	}
	return handlerStreamType{}, matches, false
}

// disambiguateByReceiver picks the candidate whose service name matches recvType
// (with or without the "Service" suffix), returning false unless exactly one does.
func disambiguateByReceiver(matches []handlerStreamType, suffix, recvType string) (handlerStreamType, bool) {
	if recvType == "" {
		return handlerStreamType{}, false
	}
	var match handlerStreamType
	count := 0
	for _, candidate := range matches {
		service := strings.TrimSuffix(candidate.name, suffix)
		if strings.EqualFold(service, recvType) ||
			strings.EqualFold(strings.TrimSuffix(service, "Service"), recvType) {
			match = candidate
			count++
		}
	}
	if count != 1 {
		return handlerStreamType{}, false
	}
	return match, true
}

// candidateNames returns the package-qualified type names, sorted.
func candidateNames(types []handlerStreamType) []string {
	names := make([]string, 0, len(types))
	for _, candidate := range types {
		qualified := candidate.name
		if candidate.pkgName != "" {
			qualified = candidate.pkgName + "." + candidate.name
		}
		names = append(names, qualified)
	}
	sort.Strings(names)
	return names
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const (
	identReceive   = "Receive"
	identSend      = "Send"
	identErr       = "err"
	identStreamErr = "Err"
)

// streamMethodNames are the v1 stream methods that mark a variable as a stream.
var streamMethodNames = map[string]bool{
	identSend:         true,
	identReceive:      true,
	"CloseSend":       true,
	"CloseRequest":    true,
	"CloseResponse":   true,
	"CloseAndReceive": true,
}

// rewriteStreams applies every streaming rewrite to one function body.
// methodName is the enclosing function's name (or "" for a closure).
func rewriteStreams(funcType *ast.FuncType, body *ast.BlockStmt, state *rewriteState, report *Report, methodName, recvName string) {
	if body == nil {
		return
	}
	streamVars, params := collectStreamVars(funcType, body, state.connectAlias)
	if len(streamVars) == 0 {
		return
	}
	for name, param := range params {
		migrated, ambiguous := migrateHandlerStreamParam(param, methodName, recvName, state, report)
		switch {
		case migrated:
			continue
		case len(ambiguous) > 0:
			report.warnAtf(param.pos, ruleStreamParamAmbiguous, "stream parameter %q (connect.%s) matches multiple v2 handler stream types because several services share this RPC name and messages: %s. Pick the one for this service by hand.", name, param.typeName, strings.Join(candidateNames(ambiguous), ", "))
		default:
			report.warnAtf(param.pos, ruleStreamParamType, "stream parameter %q has v1 type connect.%s. Its v2 type is the generated handler stream type for this RPC.", name, param.typeName)
		}
	}
	holders := map[string]bool{}
	rewriteStreamBlocks(body, streamVars, funcType, state, report, holders)
	dropMsgForHolders(body, holders, report)
	threadStreamMethods(body, streamVars, report)
	rewriteStreamMetadata(body, streamVars, params, state, report, contextParamName(funcType))
}

func isStreamMetadataMethod(name string) bool {
	return name == "RequestHeader" || name == "ResponseHeader" || name == "ResponseTrailer"
}

// rewriteStreamMetadata moves a stream's RequestHeader/ResponseHeader/
// ResponseTrailer access to the v2 CallInfo: handler streams read inline from
// the server CallInfo; client streams seed a NewClientContext.
func rewriteStreamMetadata(body *ast.BlockStmt, streamVars map[string]bool, params map[string]streamParam, state *rewriteState, report *Report, ctxName string) {
	infoName := ""
	walkFuncBody(body, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isStreamMetadataMethod(sel.Sel.Name) {
			return
		}
		recv, isIdent := sel.X.(*ast.Ident)
		if !isIdent {
			return
		}
		if _, isHandler := params[recv.Name]; !isHandler {
			return
		}
		if ctxName == "" {
			report.warnAtf(call.Pos(), ruleRequestMetadata, "stream %s.%s() moves to the connect.CallInfoForServerContext(ctx) info's %s() in v2", recv.Name, sel.Sel.Name, sel.Sel.Name)
			return
		}
		if infoName == "" {
			infoName = ensureServerCallInfo(body, state, ctxName)
		}
		call.Fun = &ast.SelectorExpr{X: ast.NewIdent(infoName), Sel: ast.NewIdent(sel.Sel.Name)}
		report.bump("stream_metadata_rewrite")
	})
	rewriteClientStreamMetadata(body, streamVars, params, state, report, ctxName)
}

// rewriteClientStreamMetadata seeds a connect.NewClientContext before a client
// stream is opened and rewrites its metadata reads to the seeded info. A single
// client stream with a usable, not-yet-seeded context qualifies; others warn.
func rewriteClientStreamMetadata(body *ast.BlockStmt, streamVars map[string]bool, params map[string]streamParam, state *rewriteState, report *Report, ctxName string) {
	const warnMsg = "client stream %s metadata moves to connect.NewClientContext(ctx) and info.RequestHeader()/ResponseHeader()/ResponseTrailer() in v2"
	withMeta := map[string]token.Pos{}
	for name := range streamVars {
		if _, isHandler := params[name]; isHandler {
			continue
		}
		if pos := firstCallPos(body, name, "RequestHeader", "ResponseHeader", "ResponseTrailer"); pos.IsValid() {
			withMeta[name] = pos
		}
	}
	if len(withMeta) == 0 {
		return
	}
	if ctxName == "" || len(withMeta) > 1 || bodyHasNewClientContext(body, state.connectV2Alias) {
		for name, pos := range withMeta {
			report.warnAtf(pos, ruleRequestMetadata, warnMsg, name)
		}
		return
	}
	var holder string
	for name := range withMeta {
		holder = name
	}
	insertAt := holderAssignIndex(body, holder)
	if insertAt < 0 {
		report.warnAtf(withMeta[holder], ruleRequestMetadata, warnMsg, holder)
		return
	}
	infoName := uniqueIdent(body, "info", "callInfo")
	seed := newClientContextSeed(state, ctxName, infoName)
	body.List = append(body.List[:insertAt], append([]ast.Stmt{seed}, body.List[insertAt:]...)...)
	report.bump("client_context_insert")
	walkFuncBody(body, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isStreamMetadataMethod(sel.Sel.Name) {
			return
		}
		if root := rootIdent(sel); root != nil && root.Name == holder {
			call.Fun = &ast.SelectorExpr{X: ast.NewIdent(infoName), Sel: ast.NewIdent(sel.Sel.Name)}
			report.bump("client_stream_metadata_rewrite")
		}
	})
}

// streamParam is a server handler stream parameter.
type streamParam struct {
	typeName string
	pos      token.Pos
	field    *ast.Field
	messages []string
}

// migrateHandlerStreamParam rewrites a handler's v1 stream parameter to the
// generated v2 handler stream type and records the import. It returns false when
// the RPC doesn't resolve, with any ambiguous matches for the caller's warning.
func migrateHandlerStreamParam(param streamParam, methodName, recvName string, state *rewriteState, report *Report) (bool, []handlerStreamType) {
	resolved, ambiguous, ok := state.handlerStreams.lookup(methodName, recvName, param.messages)
	if !ok {
		return false, ambiguous
	}
	param.field.Type = &ast.SelectorExpr{X: ast.NewIdent(resolved.pkgName), Sel: ast.NewIdent(resolved.name)}
	state.addImport(resolved.pkgPath, resolved.pkgName)
	report.bump("stream_handler_param")
	return true, nil
}

// collectStreamVars returns every stream variable name in the function, plus
// the subset that are server handler parameters.
func collectStreamVars(funcType *ast.FuncType, body *ast.BlockStmt, connectAlias string) (map[string]bool, map[string]streamParam) {
	params := collectStreamParams(funcType, connectAlias)
	vars := map[string]bool{}
	for name := range params {
		vars[name] = true
	}
	called := map[string]bool{}
	walkFuncBody(body, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !streamMethodNames[sel.Sel.Name] {
			return
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			called[id.Name] = true
		}
	})
	for name := range called {
		if !vars[name] && assignedFromMethodCall(body, name) {
			vars[name] = true
		}
	}
	return vars, params
}

// collectStreamParams maps each v1 connect stream-generic parameter to its info.
func collectStreamParams(funcType *ast.FuncType, connectAlias string) map[string]streamParam {
	out := map[string]streamParam{}
	if funcType == nil || funcType.Params == nil {
		return out
	}
	for _, field := range funcType.Params.List {
		typeName, ok := streamGenericName(field.Type, connectAlias)
		if !ok {
			continue
		}
		messages := streamTypeArgNames(field.Type)
		for _, name := range field.Names {
			out[name.Name] = streamParam{typeName: typeName, pos: name.Pos(), field: field, messages: messages}
		}
	}
	return out
}

// streamTypeArgNames returns the base names of a *connect.XxxStream[...] type's
// message type arguments, e.g. ["CumSumRequest", "CumSumResponse"].
func streamTypeArgNames(typ ast.Expr) []string {
	star, isStar := typ.(*ast.StarExpr)
	if !isStar {
		return nil
	}
	var args []ast.Expr
	switch indexed := star.X.(type) {
	case *ast.IndexExpr:
		args = []ast.Expr{indexed.Index}
	case *ast.IndexListExpr:
		args = indexed.Indices
	default:
		return nil
	}
	var names []string
	for _, arg := range args {
		switch typeArg := arg.(type) {
		case *ast.SelectorExpr:
			names = append(names, typeArg.Sel.Name)
		case *ast.Ident:
			names = append(names, typeArg.Name)
		}
	}
	return names
}

// streamGenericName reports the v1 stream type name for a
// *connect.XxxStream[...] parameter type.
func streamGenericName(typ ast.Expr, connectAlias string) (string, bool) {
	star, isStar := typ.(*ast.StarExpr)
	if !isStar {
		return "", false
	}
	var (
		sel   *ast.SelectorExpr
		isSel bool
	)
	switch indexed := star.X.(type) {
	case *ast.IndexExpr:
		sel, isSel = indexed.X.(*ast.SelectorExpr)
	case *ast.IndexListExpr:
		sel, isSel = indexed.X.(*ast.SelectorExpr)
	default:
		return "", false
	}
	if !isSel {
		return "", false
	}
	if id, isIdent := sel.X.(*ast.Ident); !isIdent || id.Name != connectAlias {
		return "", false
	}
	switch sel.Sel.Name {
	case "ClientStream", "ServerStream", "BidiStream":
		return sel.Sel.Name, true
	}
	return "", false
}

// isSelectorCall reports whether expr is a method/qualified call x.M(...).
func isSelectorCall(expr ast.Expr) bool {
	call, isCall := expr.(*ast.CallExpr)
	if !isCall {
		return false
	}
	_, isSel := call.Fun.(*ast.SelectorExpr)
	return isSel
}

// assignedFromMethodCall reports whether name is ever assigned a method call's
// result, via `:=`/`=` or a `var name = call` declaration, so the method-set
// heuristic ignores unrelated values with a Send method.
func assignedFromMethodCall(body *ast.BlockStmt, name string) bool {
	found := false
	walkFuncBody(body, func(n ast.Node) {
		if found {
			return
		}
		switch node := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				id, isIdent := lhs.(*ast.Ident)
				if !isIdent || id.Name != name {
					continue
				}
				rhs := node.Rhs[0]
				if len(node.Rhs) == len(node.Lhs) {
					rhs = node.Rhs[i]
				}
				if isSelectorCall(rhs) {
					found = true
					return
				}
			}
		case *ast.ValueSpec:
			for i, id := range node.Names {
				if id.Name == name && i < len(node.Values) && isSelectorCall(node.Values[i]) {
					found = true
					return
				}
			}
		}
	})
	return found
}

// rewriteStreamBlocks performs statement-list surgery on every block in the
// function (but not nested closures).
func rewriteStreamBlocks(body *ast.BlockStmt, streamVars map[string]bool, funcType *ast.FuncType, state *rewriteState, report *Report, holders map[string]bool) {
	walkFuncBody(body, func(n ast.Node) {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return
		}
		block.List, _ = rewriteStmtList(block.List, streamVars, funcType, state, report, holders)
	})
}

func rewriteStmtList(list []ast.Stmt, streamVars map[string]bool, funcType *ast.FuncType, state *rewriteState, report *Report, holders map[string]bool) ([]ast.Stmt, bool) {
	out := make([]ast.Stmt, 0, len(list))
	changed := false
	for i := 0; i < len(list); i++ {
		stmt := list[i]
		if newStmts, ok := tryConstructorTuple(stmt, streamVars, funcType, report); ok {
			out = append(out, newStmts...)
			changed = true
			continue
		}
		if newStmts, holder, ok := tryCloseAndReceive(stmt, streamVars, funcType, report); ok {
			out = append(out, newStmts...)
			if holder != "" {
				holders[holder] = true
			}
			changed = true
			continue
		}
		forStmt, isFor := stmt.(*ast.ForStmt)
		if !isFor {
			out = append(out, stmt)
			continue
		}
		streamName, isBoolLoop := boolLoopStreamName(forStmt, streamVars)
		if !isBoolLoop {
			out = append(out, stmt)
			continue
		}
		if i+1 < len(list) {
			if name, body, ok := matchStreamErrCheck(list[i+1], streamName); ok {
				// Foldable `if err := stream.Err(); err != nil`: it becomes the
				// loop's non-EOF error handler.
				out = append(out, reshapeBoolLoop(forStmt, streamName, name, body, funcType, state, report))
				i++
				changed = true
				continue
			}
		}
		if hasTrailingStreamErr(list[i+1:], streamName) {
			// Non-foldable post-loop stream.Err() (a success-path check or bare
			// return): hoist the terminal error so that code keeps working.
			out = append(out, reshapeBoolLoopTrailingErr(forStmt, streamName, list[i+1:], state, report)...)
			changed = true
			continue
		}
		out = append(out, reshapeBoolLoop(forStmt, streamName, identErr, nil, funcType, state, report))
		changed = true
	}
	return out, changed
}

// normalizeReshapedClosures repairs the vertical gap a position-cleared closure
// body leaves above it: it clears brace/paren positions from each cleared
// closure up to its enclosing function so the printer emits no dangling `})`
// or stray blank line.
func normalizeReshapedClosures(file *ast.File) {
	parent := parentMap(file)
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok || lit.Body == nil || lit.Body.Rbrace != token.NoPos {
			return true
		}
		for cur := ast.Node(lit); cur != nil; cur = parent[cur] {
			switch node := cur.(type) {
			case *ast.BlockStmt:
				node.Lbrace, node.Rbrace = token.NoPos, token.NoPos
			case *ast.CallExpr:
				node.Lparen, node.Rparen, node.Ellipsis = token.NoPos, token.NoPos, token.NoPos
			case *ast.FuncDecl:
				return true // reached the enclosing function, stop climbing
			}
		}
		return true
	})
}

func parentMap(file *ast.File) map[ast.Node]ast.Node {
	parent := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			parent[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})
	return parent
}

// anchorPositions pins a synthesized subtree to one existing position so the
// printer slots it next to its neighbors without a blank-line gap (unlike
// clearPositions, which can orphan nearby free-floating comments).
func anchorPositions(node ast.Node, pos token.Pos) {
	setPositions(node, pos)
}

func setPositions(node ast.Node, pos token.Pos) {
	ast.Inspect(node, func(n ast.Node) bool {
		setExprPos(n, pos)
		setStmtPos(n, pos)
		return true
	})
}

func setExprPos(n ast.Node, pos token.Pos) {
	switch node := n.(type) {
	case *ast.Ident:
		node.NamePos = pos
	case *ast.BasicLit:
		node.ValuePos = pos
	case *ast.CallExpr:
		node.Lparen, node.Rparen = pos, pos
		// Only move Ellipsis on a real spread; a position on a non-spread call
		// makes the printer emit a spurious `...`.
		if node.Ellipsis.IsValid() {
			node.Ellipsis = pos
		}
	case *ast.BinaryExpr:
		node.OpPos = pos
	case *ast.UnaryExpr:
		node.OpPos = pos
	case *ast.StarExpr:
		node.Star = pos
	case *ast.ParenExpr:
		node.Lparen, node.Rparen = pos, pos
	case *ast.IndexExpr:
		node.Lbrack, node.Rbrack = pos, pos
	case *ast.CompositeLit:
		node.Lbrace, node.Rbrace = pos, pos
	case *ast.KeyValueExpr:
		node.Colon = pos
	case *ast.FuncLit:
		node.Type.Func = pos
	}
}

func setStmtPos(n ast.Node, pos token.Pos) {
	switch node := n.(type) {
	case *ast.AssignStmt:
		node.TokPos = pos
	case *ast.ReturnStmt:
		node.Return = pos
	case *ast.BranchStmt:
		node.TokPos = pos
	case *ast.IfStmt:
		node.If = pos
	case *ast.ForStmt:
		node.For = pos
	case *ast.RangeStmt:
		node.For, node.TokPos = pos, pos
	case *ast.BlockStmt:
		node.Lbrace, node.Rbrace = pos, pos
	case *ast.IncDecStmt:
		node.TokPos = pos
	case *ast.GenDecl:
		node.TokPos, node.Lparen, node.Rparen = pos, pos, pos
	case *ast.SendStmt:
		node.Arrow = pos
	case *ast.DeferStmt:
		node.Defer = pos
	case *ast.GoStmt:
		node.Go = pos
	case *ast.LabeledStmt:
		node.Colon = pos
	case *ast.SwitchStmt:
		node.Switch = pos
	case *ast.TypeSwitchStmt:
		node.Switch = pos
	case *ast.SelectStmt:
		node.Select = pos
	case *ast.CaseClause:
		node.Case, node.Colon = pos, pos
	case *ast.CommClause:
		node.Case, node.Colon = pos, pos
	}
}

// tryConstructorTuple adds the v2 `, err` result plus an error check to a
// sole-LHS stream constructor binding, in either the `stream := client.M(ctx)`
// or the type-inferred `var stream = client.M(ctx)` form. A binding that already
// has an error result (two LHS) is left alone.
func tryConstructorTuple(stmt ast.Stmt, streamVars map[string]bool, funcType *ast.FuncType, report *Report) ([]ast.Stmt, bool) {
	switch node := stmt.(type) {
	case *ast.AssignStmt:
		if node.Tok != token.DEFINE || len(node.Lhs) != 1 || len(node.Rhs) != 1 {
			return nil, false
		}
		id, isIdent := node.Lhs[0].(*ast.Ident)
		if !isIdent || !streamVars[id.Name] || !isSelectorCall(node.Rhs[0]) {
			return nil, false
		}
		node.Lhs = append(node.Lhs, errResultIdent(node.Pos()))
	case *ast.DeclStmt:
		gen, isGen := node.Decl.(*ast.GenDecl)
		if !isGen || gen.Tok != token.VAR || len(gen.Specs) != 1 {
			return nil, false
		}
		// An explicit type would make `var stream, err T = call` ill-typed.
		spec, isSpec := gen.Specs[0].(*ast.ValueSpec)
		if !isSpec || spec.Type != nil || len(spec.Names) != 1 || len(spec.Values) != 1 {
			return nil, false
		}
		if !streamVars[spec.Names[0].Name] || !isSelectorCall(spec.Values[0]) {
			return nil, false
		}
		spec.Names = append(spec.Names, errResultIdent(node.Pos()))
	default:
		return nil, false
	}
	check := errCheck(funcType, identErr)
	anchorPositions(check, stmt.End())
	report.bump("stream_client_ctor")
	return []ast.Stmt{stmt, check}, true
}

// errResultIdent builds the err identifier appended as the new error result,
// anchored at pos so the printer keeps it on the binding's line.
func errResultIdent(pos token.Pos) *ast.Ident {
	id := ast.NewIdent(identErr)
	id.NamePos = pos
	return id
}

// tryCloseAndReceive handles `res, err := stream.CloseAndReceive()`, which v2
// keeps but with no context argument and a bare-message result. It returns the
// response holder name so its .Msg accesses can be dropped.
func tryCloseAndReceive(stmt ast.Stmt, streamVars map[string]bool, _ *ast.FuncType, report *Report) ([]ast.Stmt, string, bool) {
	assign, isAssign := stmt.(*ast.AssignStmt)
	if !isAssign || len(assign.Rhs) != 1 {
		return nil, "", false
	}
	call, isCall := assign.Rhs[0].(*ast.CallExpr)
	if !isCall {
		return nil, "", false
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != "CloseAndReceive" {
		return nil, "", false
	}
	streamName, isIdent := sel.X.(*ast.Ident)
	if !isIdent || !streamVars[streamName.Name] {
		return nil, "", false
	}
	assign.Rhs[0] = methodCall(streamName.Name, "CloseAndReceive")
	anchorPositions(assign.Rhs[0], assign.Pos())
	report.bump("stream_close_and_receive")
	holder := ""
	if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
		holder = id.Name
	}
	return []ast.Stmt{assign}, holder, true
}

// boolLoopStreamName reports the stream variable of a v1
// `for stream.Receive() { ... }` loop.
func boolLoopStreamName(forStmt *ast.ForStmt, streamVars map[string]bool) (string, bool) {
	if forStmt.Init != nil || forStmt.Post != nil || forStmt.Cond == nil {
		return "", false
	}
	call, isCall := forStmt.Cond.(*ast.CallExpr)
	if !isCall || len(call.Args) != 0 {
		return "", false
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != identReceive {
		return "", false
	}
	id, isIdent := sel.X.(*ast.Ident)
	if !isIdent || !streamVars[id.Name] {
		return "", false
	}
	return id.Name, true
}

// matchStreamErrCheck recognises an `if err := stream.Err(); err != nil` (or
// init-less `if stream.Err() != nil`) check after a v1 receive loop, returning
// the error variable name and the non-EOF handler statements.
func matchStreamErrCheck(stmt ast.Stmt, streamName string) (string, []ast.Stmt, bool) {
	ifStmt, isIf := stmt.(*ast.IfStmt)
	if !isIf || ifStmt.Body == nil {
		return "", nil, false
	}
	if init, isAssign := ifStmt.Init.(*ast.AssignStmt); isAssign {
		if len(init.Lhs) != 1 || len(init.Rhs) != 1 || !isStreamErrCall(init.Rhs[0], streamName) {
			return "", nil, false
		}
		id, isIdent := init.Lhs[0].(*ast.Ident)
		if !isIdent || !isNotNilCheck(ifStmt.Cond, id.Name) {
			return "", nil, false
		}
		replaceStreamErr(ifStmt.Body.List, streamName, id.Name)
		return id.Name, ifStmt.Body.List, true
	}
	// The `!= nil` shape is required so an `if stream.Err() == nil` success
	// block is not mistaken for the error handler.
	bin, isBinary := ifStmt.Cond.(*ast.BinaryExpr)
	if !isBinary || bin.Op != token.NEQ || !isStreamErrCall(bin.X, streamName) || !isNilIdent(bin.Y) {
		return "", nil, false
	}
	handler := ifStmt.Body.List
	replaceStreamErr(handler, streamName, identErr)
	return identErr, handler, true
}

func isNilIdent(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == identNil
}

// isNotNilCheck reports whether cond is `name != nil`.
func isNotNilCheck(cond ast.Expr, name string) bool {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.NEQ {
		return false
	}
	x, isIdent := bin.X.(*ast.Ident)
	return isIdent && x.Name == name && isNilIdent(bin.Y)
}

func isStreamErrCall(expr ast.Expr, streamName string) bool {
	call, isCall := expr.(*ast.CallExpr)
	if !isCall || len(call.Args) != 0 {
		return false
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != identStreamErr {
		return false
	}
	id, isIdent := sel.X.(*ast.Ident)
	return isIdent && id.Name == streamName
}

// replaceStreamErr rewrites every stream.Err() call in stmts to errName, since
// the reshaped v2 loop has no Err() method.
func replaceStreamErr(stmts []ast.Stmt, streamName, errName string) {
	for _, stmt := range stmts {
		astutil.Apply(stmt, nil, func(c *astutil.Cursor) bool {
			if expr, ok := c.Node().(ast.Expr); ok && isStreamErrCall(expr, streamName) {
				c.Replace(ast.NewIdent(errName))
			}
			return true
		})
	}
}

// hasTrailingStreamErr reports whether any statement references stream.Err().
func hasTrailingStreamErr(stmts []ast.Stmt, streamName string) bool {
	for _, stmt := range stmts {
		found := false
		ast.Inspect(stmt, func(n ast.Node) bool {
			if found {
				return false
			}
			if expr, isExpr := n.(ast.Expr); isExpr && isStreamErrCall(expr, streamName) {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// replaceStreamErrExprs rewrites every stream.Err() call to errName in place,
// in any position (not just return results, unlike replaceStreamErr).
func replaceStreamErrExprs(stmts []ast.Stmt, streamName, errName string) {
	for _, stmt := range stmts {
		astutil.Apply(stmt, nil, func(cursor *astutil.Cursor) bool {
			if expr, isExpr := cursor.Node().(ast.Expr); isExpr && isStreamErrCall(expr, streamName) {
				cursor.Replace(ast.NewIdent(errName))
			}
			return true
		})
	}
}

// reshapeBoolLoopTrailingErr reshapes a v1 receive loop whose terminal error is
// inspected afterwards in a form matchStreamErrCheck can't fold (a success-path
// `if stream.Err() == nil`, a bare `return stream.Err()`). It hoists
// `var <streamErr> error` before the loop, captures the receive error and breaks,
// normalizes io.EOF to nil (v1 stream.Err() is nil on clean completion), and
// rewrites the post-loop stream.Err() references in rest (mutated in place).
func reshapeBoolLoopTrailingErr(forStmt *ast.ForStmt, streamName string, rest []ast.Stmt, state *rewriteState, report *Report) []ast.Stmt {
	msgName := "_"
	if hasStreamMsg(forStmt.Body, streamName) {
		msgName = uniqueIdent(forStmt.Body, "msg")
		replaceStreamMsg(forStmt.Body, streamName, msgName)
	}
	// The hoisted error spans the loop and the post-loop code, so it must be
	// unique across both; the loop-local receive error stays "err".
	scope := &ast.BlockStmt{List: append(append([]ast.Stmt(nil), forStmt.Body.List...), rest...)}
	streamErrName := uniqueIdent(scope, "streamErr")
	localErr := uniqueIdent(forStmt.Body, identErr)

	recv := &ast.AssignStmt{
		Lhs: []ast.Expr{ast.NewIdent(msgName), ast.NewIdent(localErr)},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{methodCall(streamName, identReceive)},
	}
	captureBreak := &ast.IfStmt{
		Cond: notNil(localErr),
		Body: &ast.BlockStmt{List: []ast.Stmt{
			&ast.AssignStmt{Lhs: []ast.Expr{ast.NewIdent(streamErrName)}, Tok: token.ASSIGN, Rhs: []ast.Expr{ast.NewIdent(localErr)}},
			&ast.BranchStmt{Tok: token.BREAK},
		}},
	}
	declSpec := &ast.GenDecl{
		Tok:   token.VAR,
		Specs: []ast.Spec{&ast.ValueSpec{Names: []*ast.Ident{ast.NewIdent(streamErrName)}, Type: ast.NewIdent("error")}},
	}
	decl := &ast.DeclStmt{Decl: declSpec}
	normalize := &ast.IfStmt{
		Cond: &ast.CallExpr{
			Fun:  &ast.SelectorExpr{X: ast.NewIdent("errors"), Sel: ast.NewIdent("Is")},
			Args: []ast.Expr{ast.NewIdent(streamErrName), &ast.SelectorExpr{X: ast.NewIdent("io"), Sel: ast.NewIdent("EOF")}},
		},
		Body: &ast.BlockStmt{List: []ast.Stmt{
			&ast.AssignStmt{Lhs: []ast.Expr{ast.NewIdent(streamErrName)}, Tok: token.ASSIGN, Rhs: []ast.Expr{ast.NewIdent(identNil)}},
		}},
	}

	replaceStreamErrExprs(rest, streamName, streamErrName)

	state.usedErrors = true
	state.usedIO = true
	report.bump("stream_recv_loop")
	report.bump("stream_err_after_loop")

	anchorPositions(decl, forStmt.For)
	// Clear the parens anchoring set, else the single var prints as a group.
	declSpec.Lparen, declSpec.Rparen = token.NoPos, token.NoPos
	anchorPositions(recv, forStmt.For)
	anchorPositions(captureBreak, forStmt.For)
	anchorPositions(normalize, forStmt.Body.Rbrace)
	newFor := &ast.ForStmt{
		For:  forStmt.For,
		Body: &ast.BlockStmt{Lbrace: forStmt.Body.Lbrace, List: append([]ast.Stmt{recv, captureBreak}, forStmt.Body.List...), Rbrace: forStmt.Body.Rbrace},
	}
	return []ast.Stmt{decl, newFor, normalize}
}

// renameStmtsIdent renames identifier references oldName to newName across
// stmts, leaving selector field names untouched.
func renameStmtsIdent(stmts []ast.Stmt, oldName, newName string) {
	for _, stmt := range stmts {
		renameIdent(stmt, oldName, newName)
	}
}

func renameIdent(node ast.Node, oldName, newName string) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch typed := n.(type) {
		case *ast.SelectorExpr:
			renameIdent(typed.X, oldName, newName) // receiver only, not the field
			return false
		case *ast.Ident:
			if typed.Name == oldName {
				typed.Name = newName
			}
		}
		return true
	})
}

// reshapeBoolLoop turns a v1 `for stream.Receive() { ... }` loop into a v2
// `for { msg, err := stream.Receive() ... }` loop with an inline error check.
func reshapeBoolLoop(forStmt *ast.ForStmt, streamName, errName string, handler []ast.Stmt, funcType *ast.FuncType, state *rewriteState, report *Report) ast.Stmt {
	msgName := "_"
	if hasStreamMsg(forStmt.Body, streamName) {
		msgName = uniqueIdent(forStmt.Body, "msg")
		replaceStreamMsg(forStmt.Body, streamName, msgName)
	}
	// If the body already binds errName, pick a fresh name to avoid redeclaring
	// it and rename the handler's references to match.
	if uniqueIdent(forStmt.Body, errName) != errName {
		scope := &ast.BlockStmt{List: append(append([]ast.Stmt(nil), forStmt.Body.List...), handler...)}
		unique := uniqueIdent(scope, errName)
		renameStmtsIdent(handler, errName, unique)
		errName = unique
	}
	if handler == nil {
		handler = defaultStreamErrHandler(funcType, errName)
	}
	recv := &ast.AssignStmt{
		Lhs: []ast.Expr{ast.NewIdent(msgName), ast.NewIdent(errName)},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{methodCall(streamName, identReceive)},
	}
	eofBreak := &ast.IfStmt{
		Cond: &ast.CallExpr{
			Fun:  &ast.SelectorExpr{X: ast.NewIdent("errors"), Sel: ast.NewIdent("Is")},
			Args: []ast.Expr{ast.NewIdent(errName), &ast.SelectorExpr{X: ast.NewIdent("io"), Sel: ast.NewIdent("EOF")}},
		},
		Body: &ast.BlockStmt{List: []ast.Stmt{&ast.BranchStmt{Tok: token.BREAK}}},
	}
	errIf := &ast.IfStmt{
		Cond: notNil(errName),
		Body: &ast.BlockStmt{List: append([]ast.Stmt{eofBreak}, handler...)},
	}
	state.usedErrors = true
	state.usedIO = true
	report.bump("stream_recv_loop")
	anchorPositions(recv, forStmt.For)
	anchorPositions(errIf, forStmt.For)
	newBody := append([]ast.Stmt{recv, errIf}, forStmt.Body.List...)
	return &ast.ForStmt{
		For:  forStmt.For,
		Body: &ast.BlockStmt{Lbrace: forStmt.Body.Lbrace, List: newBody, Rbrace: forStmt.Body.Rbrace},
	}
}

func hasStreamMsg(body *ast.BlockStmt, streamName string) bool {
	found := false
	walkFuncBody(body, func(n ast.Node) {
		if found || !isStreamMsgCall(n, streamName) {
			return
		}
		found = true
	})
	return found
}

func replaceStreamMsg(body *ast.BlockStmt, streamName, msgName string) {
	replace := func(expr *ast.Expr) {
		if isStreamMsgCall(*expr, streamName) {
			*expr = ast.NewIdent(msgName)
		}
	}
	walkFuncBody(body, func(n ast.Node) {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			replace(&node.X)
		case *ast.CallExpr:
			replace(&node.Fun)
			for i := range node.Args {
				replace(&node.Args[i])
			}
		case *ast.AssignStmt:
			for i := range node.Rhs {
				replace(&node.Rhs[i])
			}
		case *ast.BinaryExpr:
			replace(&node.X)
			replace(&node.Y)
		case *ast.IndexExpr:
			replace(&node.X)
			replace(&node.Index)
		case *ast.ReturnStmt:
			for i := range node.Results {
				replace(&node.Results[i])
			}
		}
	})
}

func isStreamMsgCall(node ast.Node, streamName string) bool {
	call, isCall := node.(*ast.CallExpr)
	if !isCall || len(call.Args) != 0 {
		return false
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != identMsg {
		return false
	}
	id, isIdent := sel.X.(*ast.Ident)
	return isIdent && id.Name == streamName
}

// threadStreamMethods renames CloseRequest/CloseResponse to CloseSend/Close on
// stream variables. (Send/Receive keep their v1 shape; the loop reshape handles
// the bool-to-error Receive change.)
func threadStreamMethods(body *ast.BlockStmt, streamVars map[string]bool, report *Report) {
	walkFuncBody(body, func(n ast.Node) {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			return
		}
		id, isIdent := sel.X.(*ast.Ident)
		if !isIdent || !streamVars[id.Name] {
			return
		}
		switch sel.Sel.Name {
		case "CloseRequest":
			sel.Sel = ast.NewIdent("CloseSend")
			report.bump("stream_close_rename")
		case "CloseResponse":
			sel.Sel = ast.NewIdent("Close")
			report.bump("stream_close_rename")
		}
	})
}

// dropMsgForHolders removes .Msg from selector chains and bare references for
// the named response holders (such as a CloseAndReceive response).
func dropMsgForHolders(body *ast.BlockStmt, holders map[string]bool, report *Report) {
	if len(holders) == 0 {
		return
	}
	walkFuncBody(body, func(n ast.Node) {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		if !ok || inner.Sel.Name != identMsg {
			return
		}
		root := rootIdent(inner)
		if root == nil || !holders[root.Name] {
			return
		}
		sel.X = inner.X
		report.bump("body_drop_msg")
	})
	rewriteBareMsg(body, holders, report)
}

// defaultStreamErrHandler builds the non-EOF receive-error handler: return the
// error, or t.Fatal it for a test function with no error result.
func defaultStreamErrHandler(funcType *ast.FuncType, errName string) []ast.Stmt {
	if !hasResults(funcType) {
		if name := testingParamName(funcType); name != "" {
			return []ast.Stmt{&ast.ExprStmt{X: methodCall(name, "Fatal", ast.NewIdent(errName))}}
		}
	}
	return []ast.Stmt{&ast.ReturnStmt{Results: zeroReturnResults(funcType, errName)}}
}

func hasResults(funcType *ast.FuncType) bool {
	return funcType != nil && funcType.Results != nil && len(funcType.Results.List) > 0
}

// testingParamName returns the name of a *testing.{T,B,F}/testing.TB parameter,
// or "".
func testingParamName(funcType *ast.FuncType) string {
	if funcType == nil || funcType.Params == nil {
		return ""
	}
	for _, field := range funcType.Params.List {
		if !isTestingType(field.Type) || len(field.Names) == 0 {
			continue
		}
		if name := field.Names[0].Name; name != "_" {
			return name
		}
	}
	return ""
}

func isTestingType(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != "testing" {
		return false
	}
	switch sel.Sel.Name {
	case "T", "B", "F", "TB":
		return true
	}
	return false
}

// errCheck builds `if err != nil { <handler> }` (see defaultStreamErrHandler).
func errCheck(funcType *ast.FuncType, errName string) ast.Stmt {
	return &ast.IfStmt{
		Cond: notNil(errName),
		Body: &ast.BlockStmt{List: defaultStreamErrHandler(funcType, errName)},
	}
}

func notNil(name string) ast.Expr {
	return &ast.BinaryExpr{X: ast.NewIdent(name), Op: token.NEQ, Y: ast.NewIdent(identNil)}
}

func methodCall(recv, method string, args ...ast.Expr) *ast.CallExpr {
	return &ast.CallExpr{
		Fun:  &ast.SelectorExpr{X: ast.NewIdent(recv), Sel: ast.NewIdent(method)},
		Args: args,
	}
}

// zeroReturnResults builds an error-return result list: a zero value for every
// non-error result, errName in the trailing error position.
func zeroReturnResults(funcType *ast.FuncType, errName string) []ast.Expr {
	if funcType == nil || funcType.Results == nil || len(funcType.Results.List) == 0 {
		return []ast.Expr{ast.NewIdent(errName)}
	}
	var types []ast.Expr
	for _, field := range funcType.Results.List {
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		for range count {
			types = append(types, field.Type)
		}
	}
	out := make([]ast.Expr, len(types))
	for i, typ := range types {
		if i == len(types)-1 && isErrorType(typ) {
			out[i] = ast.NewIdent(errName)
			continue
		}
		out[i] = zeroValueExpr(typ)
	}
	return out
}

func isErrorType(typ ast.Expr) bool {
	id, ok := typ.(*ast.Ident)
	return ok && id.Name == "error"
}

func zeroValueExpr(typ ast.Expr) ast.Expr {
	ident, ok := typ.(*ast.Ident)
	if !ok {
		return ast.NewIdent(identNil) // pointers, slices, maps, chans, funcs, interfaces
	}
	switch ident.Name {
	case "string":
		return &ast.BasicLit{Kind: token.STRING, Value: `""`}
	case "bool":
		return ast.NewIdent("false")
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"byte", "rune", "float32", "float64", "complex64", "complex128":
		return &ast.BasicLit{Kind: token.INT, Value: "0"}
	default:
		return ast.NewIdent(identNil) // named types fall back to nil; may need a manual fix
	}
}
