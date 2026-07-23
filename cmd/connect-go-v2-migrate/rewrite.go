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
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"strings"

	connect "connectrpc.com/connect/v2"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/imports"
)

const (
	identMsg    = "Msg"
	identError  = "Error"
	identErrorf = "Errorf"
	identHeader = "Header"
	identNil    = "nil"
)

const (
	// WarningManual is work the user must do by hand, printed with its message.
	WarningManual WarningKind = iota
	// WarningDeferred is a stub-dependent rewrite, printed as a bare position.
	WarningDeferred
)

// Diagnostic rule identifiers, surfaced as stable handles in JSON output.
const (
	ruleAwaitingV2Bindings   = "awaiting_v2_bindings"
	ruleRemoveV1Import       = "remove_v1_import"
	ruleRequestMetadata      = "request_metadata_migration"
	ruleHandlerConstruction  = "handler_construction"
	ruleConnectHTTPOption    = "connecthttp_option_migration"
	ruleErrorAPI             = "error_api_migration"
	ruleServerInterceptor    = "server_interceptor_migration"
	ruleInterceptorMigration = "interceptor_migration"
	ruleStreamParamType      = "stream_param_type"
	ruleStreamParamAmbiguous = "stream_param_ambiguous"
	ruleEcosystemMigration   = "ecosystem_migration"
	ruleBufgenReinstall      = "bufgen_reinstall_plugin"
	ruleBufgenGoMod          = "bufgen_update_go_mod"
)

var (
	// optionTypeNames are the v1 connect option interface types that v2 unified
	// into the single connecthttp.Option.
	optionTypeNames = map[string]bool{
		"HandlerOption": true,
		"ClientOption":  true,
		"Option":        true,
	}
	// movedToConnectHTTP lists v1 connect symbols that relocated to connecthttp
	// verbatim, so connect.<name> becomes connecthttp.<name>.
	movedToConnectHTTP = map[string]bool{
		"WithCompressMinBytes":             true,
		"WithReadMaxBytes":                 true,
		"WithSendMaxBytes":                 true,
		"WithRequireConnectProtocolHeader": true,
		"WithSendGzip":                     true,
		"WithSendCompression":              true,
		"WithHTTPGet":                      true,
		"WithHTTPGetMaxURLSize":            true,
		"WithProtoJSON":                    true,
		"WithCodec":                        true,
		"ErrorWriter":                      true,
		"NewErrorWriter":                   true,
		"IsNotModifiedError":               true,
	}
	// reshapedToConnectHTTP maps v1 connect symbols whose connecthttp v2 form
	// changed name or signature, so the tool warns instead of rewriting.
	reshapedToConnectHTTP = map[string]string{
		"HandlerOption":                 "connecthttp.Option (interceptors go to connect.NewServer, HTTP options to connecthttp.Mount)",
		"ClientOption":                  "connecthttp.Option (interceptors go to connect.NewClient, HTTP options to connecthttp.NewTransport)",
		"WithAcceptCompression":         "connecthttp.WithCompressor to register a connect.Compressor (see connectgzip), then connecthttp.WithAcceptCompression(name) to advertise it",
		"WithCompression":               "connecthttp.WithCompressor(connect.Compressor) (see connectgzip); the (name, decompressor, compressor) signature changed",
		"WithConditionalHandlerOptions": "connecthttp.WithConditionalOptions(func(connect.Spec) []connecthttp.Option); the callback signature changed",
		"NewNotModifiedError":           "connecthttp.NewNotModifiedError (takes no http.Header argument)",
	}
	// connectProtocolOptions is the set of v1 protocol-selecting options. They keep
	// their names in connecthttp (connect.WithGRPC() becomes connecthttp.WithGRPC()),
	// so only the package qualifier changes.
	connectProtocolOptions = map[string]string{
		"WithGRPC":    connect.ProtocolNameGRPC,
		"WithGRPCWeb": connect.ProtocolNameGRPCWeb,
	}

	// reshapedErrorAPI maps v1 connect error helpers that v2 folded into *connect.Error
	// methods. They stay in core but changed shape, so the tool warns.
	reshapedErrorAPI = map[string]string{
		"NewErrorDetail": "connectproto.NewErrorDetail(msg), then attach with (*connect.Error).WithDetail(detail)",
		"IsWireError":    "errors.As(err, &cerr) into a *connect.Error, then cerr.IsRemote()",
		"NewWireError":   "connect.NewError(code, msg).WithRemote()",
	}

	// reshapedConstruction maps v1 connect options that became positional args to
	// connect.NewServer / connect.NewClient in v2, so the tool warns.
	reshapedConstruction = map[string]string{
		"WithInterceptors": "interceptors pass to connect.NewServer(...) or connect.NewClient(...). The v2 type is connect.ServerInterceptor or connect.ClientInterceptor.",
		"WithRecover":      "reimplement as a connect.ServerInterceptor that recovers panics. Re-panic http.ErrAbortHandler so the server still aborts the response.",
	}

	// renamedConnectCore lists v1 connect symbols that v2 renamed but kept in the
	// core package, so only the selector changes.
	renamedConnectCore = map[string]string{
		"CallInfoForHandlerContext": "CallInfoForServerContext",
	}
)

// WarningKind groups diagnostics for the sectioned output.
type WarningKind int

// Warning is a diagnostic anchored to a source position (file:line:col), or
// position-less when File is empty.
type Warning struct {
	File string
	Line int
	Col  int
	Msg  string
	Kind WarningKind
	Rule string
}

// Report describes what a [Rewrite] invocation did to a single file.
type Report struct {
	Changed  bool
	Counts   map[string]int // transformation name -> times fired
	Warnings []Warning      // patterns detected but not rewritten
	// fset resolves positions for warnAt; nil for non-Go reports (buf.gen.yaml).
	fset *token.FileSet
}

// Summary returns a stable, single-line summary of the counts.
func (r *Report) Summary() string {
	if !r.Changed {
		return "unchanged"
	}
	keys := make([]string, 0, len(r.Counts))
	for k := range r.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, r.Counts[k]))
	}
	return strings.Join(parts, " ")
}

func (r *Report) bump(name string) {
	if r.Counts == nil {
		r.Counts = map[string]int{}
	}
	r.Counts[name]++
	r.Changed = true
}

// warnAtf records a diagnostic anchored at pos (file:line:col when resolvable).
func (r *Report) warnAtf(pos token.Pos, rule, format string, args ...any) {
	warning := Warning{Rule: rule, Msg: fmt.Sprintf(format, args...)}
	if r.fset != nil && pos.IsValid() {
		position := r.fset.Position(pos)
		warning.File, warning.Line, warning.Col = position.Filename, position.Line, position.Column
	}
	r.Warnings = append(r.Warnings, warning)
}

// warnedAt reports whether a warning was already recorded at pos, so a later
// pass can avoid emitting a duplicate diagnostic for the same call site.
func (r *Report) warnedAt(pos token.Pos) bool {
	if r.fset == nil || !pos.IsValid() {
		return false
	}
	position := r.fset.Position(pos)
	for _, warning := range r.Warnings {
		if warning.File == position.Filename && warning.Line == position.Line && warning.Col == position.Column {
			return true
		}
	}
	return false
}

// deferAtf records a deferred (stub-dependent) diagnostic anchored at pos.
func (r *Report) deferAtf(pos token.Pos, rule, format string, args ...any) {
	before := len(r.Warnings)
	r.warnAtf(pos, rule, format, args...)
	r.Warnings[before].Kind = WarningDeferred
}

// warnAtLinef records a diagnostic at an explicit file and 1-based line.
func (r *Report) warnAtLinef(file string, line int, rule, format string, args ...any) {
	r.Warnings = append(r.Warnings, Warning{File: file, Line: line, Col: 1, Rule: rule, Msg: fmt.Sprintf(format, args...)})
}

// rewriteOption configures an optional input to Rewrite.
type rewriteOption func(*rewriteState)

// withHandlerStreams supplies the resolved handler stream types.
func withHandlerStreams(resolver *handlerStreamResolver) rewriteOption {
	return func(state *rewriteState) { state.handlerStreams = resolver }
}

// Rewrite applies the v1->v2 mechanical rewrites to src, returning the output
// and a [Report]. Errors are syntax errors only; type mismatches surface as
// warnings. When stubsReady is false (generated bindings are still v1),
// stub-dependent rewrites are deferred and flagged.
func Rewrite(filename string, src []byte, stubsReady bool, opts ...rewriteOption) ([]byte, Report, error) {
	report := Report{}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, report, err
	}
	report.fset = fset

	state := newRewriteState(file)
	state.stubsReady = stubsReady
	for _, opt := range opts {
		opt(state)
	}
	// Nothing to do unless the file uses connect directly, via a generated
	// stub, or via an ecosystem package.
	if !state.hasConnectV1Import() && !usesConnectStub(file) && !hasEcosystemImport(file) {
		return src, report, nil
	}

	// Stub-dependent rewrites wait for v2 bindings; flag the file meanwhile.
	if !state.stubsReady {
		if pos, ok := firstStubDependentPos(file, state.connectAlias); ok {
			report.deferAtf(pos, ruleAwaitingV2Bindings, "stub-dependent rewrite (handler/client signatures, .Msg, NewRequest/NewResponse, streams, construction)")
		}
		if pos := firstEcosystemImportPos(file); pos.IsValid() {
			report.deferAtf(pos, ruleAwaitingV2Bindings, "ecosystem package rewrite (/v2 import paths, construction)")
		}
		rewriteStubIndependent(file, state, &report)
		return finishRewrite(filename, src, fset, file, state, &report)
	}

	// Process every function-bearing node, merging the outer scope's unwrap set
	// into each closure (minus names it shadows), so `req.Msg` referenced from a
	// callback is still unwrapped.
	processed := map[*ast.FuncLit]bool{}
	var processFunc func(funcType *ast.FuncType, body *ast.BlockStmt, methodName, recvName string, outerMsg, outerServerReqs map[string]bool)
	processFunc = func(funcType *ast.FuncType, body *ast.BlockStmt, methodName, recvName string, outerMsg, outerServerReqs map[string]bool) {
		// serverReqs are *connect.Request[T] params; their .Header()/.Spec() map
		// to the server CallInfo. Client response holders also lose .Msg but read
		// response metadata, so the two sets stay apart.
		serverReqs := rewriteFuncSignature(funcType, state, &report)
		if body == nil {
			return
		}
		clientRequests := scanClientRequestHolders(body, state.connectAlias)
		clientResponses := scanClientResponseHolders(body, state.connectAlias, clientRequests)
		msgHolders := map[string]bool{}
		for name := range serverReqs {
			msgHolders[name] = true
		}
		for name := range clientResponses {
			msgHolders[name] = true
		}
		mergedMsg := mergeUnwrappedScopes(outerMsg, funcType, msgHolders)
		mergedServerReqs := mergeUnwrappedScopes(outerServerReqs, funcType, serverReqs)
		rewriteFuncBody(body, state, &report, mergedMsg, mergedServerReqs, clientRequests, clientResponses, contextParamName(funcType))
		rewriteStreams(funcType, body, state, &report, methodName, recvName)
		// Recurse into nested closures with the merged sets as their outer scope,
		// marking them processed so the file-level walk skips them.
		walkFuncBody(body, func(n ast.Node) {
			lit, ok := n.(*ast.FuncLit)
			if !ok || lit.Body == nil {
				return
			}
			processed[lit] = true
			processFunc(lit.Type, lit.Body, "", "", mergedMsg, mergedServerReqs)
		})
	}
	walk(file, func(n ast.Node) {
		switch node := n.(type) {
		case *ast.FuncDecl:
			processFunc(node.Type, node.Body, node.Name.Name, receiverTypeName(node.Recv), nil, nil)
		case *ast.FuncLit:
			if processed[node] {
				return
			}
			processFunc(node.Type, node.Body, "", "", nil, nil)
		case *ast.InterfaceType:
			// Interface methods have a signature but no body.
			for _, field := range node.Methods.List {
				if funcType, ok := field.Type.(*ast.FuncType); ok {
					rewriteFuncSignature(funcType, state, &report)
				}
			}
		}
	})

	// Flip the connect.Request[T]/Response[T] types rewriteFuncSignature didn't
	// reach: function-type literals, struct fields, var and type declarations.
	flipMessageWrapperTypes(file, state, &report)

	// Reshape server/client construction to the v2 *connect.Server / *connect.Client
	// forms. Runs before the package-split pass so relocated options still flip.
	rewriteServerConstruction(file, state, &report)
	rewriteClientConstruction(file, state, &report)

	// Warn on ecosystem call sites the construction passes didn't reshape. Runs
	// after them so already-renamed interceptors aren't re-flagged.
	warnEcosystemCalls(file, &report)

	rewriteStubIndependent(file, state, &report)
	return finishRewrite(filename, src, fset, file, state, &report)
}

// rewriteStubIndependent applies the rewrites that don't depend on the
// generated stub API: NewResponse/NewRequest stripping, NewError/Code/Errorf
// translation, residual qualifier flips, and the package-split option moves.
func rewriteStubIndependent(file *ast.File, state *rewriteState, report *Report) {
	// Rename HTTP-only []connect.XOption helper types first, so the moved-symbol
	// pass no longer sees a connect.HandlerOption selector to warn about.
	renameHTTPOnlyOptionTypes(file, state, report)

	// Collapse the NewErrorDetail/AddDetail guard into WithDetail first, so it
	// isn't also reported as an unported reshaped symbol.
	rewriteErrorDetails(file, state, report)

	// File-wide expression rewrites (NewResponse/NewRequest/Code*/NewError/Errorf);
	// many occur outside function bodies.
	rewriteFileExprs(file, state, report)

	// Flip any remaining qualifier-only `connect.<X>` selectors the
	// position-specific passes above missed.
	flipRemainingConnectSelectors(file, state, report)

	rewriteMovedSymbols(file, state, report)
}

// finishRewrite finalizes imports, prints the file, and runs goimports,
// warning (with the residual symbols) if the v1 import survived.
func finishRewrite(filename string, src []byte, fset *token.FileSet, file *ast.File, state *rewriteState, report *Report) ([]byte, Report, error) {
	if state.usedV2 {
		ensureConnectV2Import(fset, file, state)
		report.bump("import_add_connectv2")
	}
	if state.usedConnectHTTP {
		ensureConnectHTTPImport(fset, file, state)
		report.bump("import_add_connecthttp")
	}
	// Stream reshapes introduce errors.Is / io.EOF (AddImport is a no-op when present).
	if state.usedErrors {
		astutil.AddImport(fset, file, "errors")
	}
	if state.usedIO {
		astutil.AddImport(fset, file, "io")
	}
	// Imports a rewrite introduced (the connect package of a handler stream type);
	// name it only when the package name differs from the path's last segment.
	for path, name := range state.imports {
		if name == path[strings.LastIndex(path, "/")+1:] {
			astutil.AddImport(fset, file, path)
		} else {
			astutil.AddNamedImport(fset, file, name, path)
		}
	}
	rewriteEcosystemImports(fset, file, state, report)
	// Drop the v1 import once v2 took over its "connect" name or the alias is no
	// longer referenced. Leftover connect.X (warned-only reshaped symbols) keeps
	// it so the file still compiles against v1 until they are ported.
	if state.hadV1Import &&
		((state.usedV2 && state.connectAlias == state.connectV2Alias) ||
			!fileReferencesIdent(file, state.connectAlias)) {
		removeConnectV1Import(fset, file, state)
		report.bump("import_drop_v1")
	}
	// Capture residual v1 symbols before printing so the retained-import warning
	// can name what's left.
	residual := residualConnectSymbols(file, state.connectAlias)

	if !report.Changed {
		warnIfV1Retained(report, file, state.hadV1Import, residual)
		return src, *report, nil
	}

	// Repair the vertical gap a position-cleared closure body leaves above it.
	normalizeReshapedClosures(file)

	var buf bytes.Buffer
	printerConfig := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	if err := (&printerConfig).Fprint(&buf, fset, file); err != nil {
		return nil, *report, fmt.Errorf("print: %w", err)
	}
	// goimports sorts/groups imports and drops unused ones. FormatOnly stays off
	// but we never ask it to ADD imports (we add what we need explicitly), so it
	// won't reach into the user environment for synthetic test inputs.
	out, err := imports.Process(filename, buf.Bytes(), &imports.Options{
		Comments:   true,
		TabIndent:  true,
		TabWidth:   8,
		Fragment:   false,
		FormatOnly: false,
	})
	if err != nil {
		return nil, *report, fmt.Errorf("imports.Process: %w", err)
	}
	warnIfV1Retained(report, file, importsConnectV1(out), residual)
	if bytes.Equal(out, src) {
		// Formatter normalized the input with no net change; keep warnings only.
		return src, Report{Warnings: report.Warnings}, nil
	}
	return out, *report, nil
}

// importsConnectV1 reports whether out still imports v1 connectrpc.com/connect
// (the trailing quote excludes the /v2 path).
func importsConnectV1(out []byte) bool {
	return bytes.Contains(out, []byte(`"connectrpc.com/connect"`))
}

// warnIfV1Retained warns, at the surviving v1 import, naming the residual
// symbols left to port by hand.
func warnIfV1Retained(report *Report, file *ast.File, retained bool, residual []string) {
	if !retained {
		return
	}
	references := "has remaining v1 references"
	if len(residual) > 0 {
		references = "still uses " + strings.Join(residual, ", ")
	}
	report.warnAtf(v1ImportPos(file), ruleRemoveV1Import, "connectrpc.com/connect (v1) import retained because it %s.", references)
}

// v1ImportPos returns the position of the v1 connect import spec, or NoPos.
func v1ImportPos(file *ast.File) token.Pos {
	for _, spec := range file.Imports {
		if path, err := strconv.Unquote(spec.Path.Value); err == nil && path == "connectrpc.com/connect" {
			return spec.Pos()
		}
	}
	return token.NoPos
}

// rewriteState tracks per-file import aliases, which imports are present, and
// which v2/connecthttp references a run introduced.
type rewriteState struct {
	connectAlias     string // v1 connect package name (default "connect")
	connectV2Alias   string // v2 connect package name (default "connect")
	connectHTTPAlias string // connecthttp package name (default "connecthttp")
	hadV1Import      bool
	hadV2Import      bool
	hadConnectHTTP   bool
	usedV2           bool // a rewrite produced a v2 connect reference
	usedConnectHTTP  bool // a rewrite produced a connecthttp reference
	usedErrors       bool // a stream reshape introduced errors.Is
	usedIO           bool // a stream reshape introduced io.EOF
	stubsReady       bool // generated bindings are v2; stub-dependent rewrites may run
	// handlerStreams resolves a handler RPC name to its v2 stream type; nil on
	// the AST-only path, where the stream parameter is warned instead.
	handlerStreams *handlerStreamResolver
	// imports maps path->package name for imports a rewrite introduces;
	// finishRewrite adds them before formatting.
	imports map[string]string
}

// addImport records that path (named pkg) must be imported.
func (s *rewriteState) addImport(path, pkg string) {
	if s.imports == nil {
		s.imports = map[string]string{}
	}
	s.imports[path] = pkg
}

func newRewriteState(file *ast.File) *rewriteState {
	state := &rewriteState{
		connectAlias:     "connect",
		connectV2Alias:   "connect",
		connectHTTPAlias: "connecthttp",
	}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		switch path {
		case "connectrpc.com/connect":
			state.hadV1Import = true
			if imp.Name != nil && imp.Name.Name != "_" && imp.Name.Name != "." {
				state.connectAlias = imp.Name.Name
			}
		case "connectrpc.com/connect/v2":
			state.hadV2Import = true
			if imp.Name != nil && imp.Name.Name != "_" && imp.Name.Name != "." {
				state.connectV2Alias = imp.Name.Name
			}
		case "connectrpc.com/connect/v2/connecthttp":
			state.hadConnectHTTP = true
			if imp.Name != nil && imp.Name.Name != "_" && imp.Name.Name != "." {
				state.connectHTTPAlias = imp.Name.Name
			}
		}
	}
	return state
}

func (s *rewriteState) hasConnectV1Import() bool {
	return s.hadV1Import
}

// rewriteFuncSignature unwraps *connect.Request[T] params to *T and
// *connect.Response[U] results to *U. Returns the set of parameter names
// whose types were unwrapped so the body pass can drop their .Msg accesses.
func rewriteFuncSignature(funcType *ast.FuncType, state *rewriteState, report *Report) map[string]bool {
	unwrapped := map[string]bool{}
	if funcType.Params != nil {
		for _, field := range funcType.Params.List {
			if newType, ok := unwrapConnectGeneric(field.Type, state.connectAlias, "Request"); ok {
				field.Type = newType
				report.bump("param_unwrap_request")
				for _, name := range field.Names {
					unwrapped[name.Name] = true
				}
			}
		}
	}
	if funcType.Results != nil {
		for _, field := range funcType.Results.List {
			if newType, ok := unwrapConnectGeneric(field.Type, state.connectAlias, "Response"); ok {
				field.Type = newType
				report.bump("result_unwrap_response")
			}
		}
	}
	return unwrapped
}

// flipMessageWrapperTypes replaces every connect.Request[T]/Response[T] type
// with T, in the positions rewriteFuncSignature doesn't reach. Composite-literal
// types are skipped (rewriteExpr unwraps those to the inner value).
func flipMessageWrapperTypes(file *ast.File, state *rewriteState, report *Report) {
	astutil.Apply(file, func(cursor *astutil.Cursor) bool {
		arg, ok := messageWrapperArg(cursor.Node(), state.connectAlias)
		if !ok {
			return true
		}
		if lit, ok := cursor.Parent().(*ast.CompositeLit); ok && lit.Type == cursor.Node() {
			return true
		}
		cursor.Replace(arg)
		report.bump("flip_message_wrapper")
		return true
	}, nil)
}

// messageWrapperArg returns the message type argument of a connect.Request[T] or
// connect.Response[T] index expression.
func messageWrapperArg(node ast.Node, connectAlias string) (ast.Expr, bool) {
	index, ok := node.(*ast.IndexExpr)
	if !ok {
		return nil, false
	}
	if isConnectSelector(index.X, connectAlias, "Request") || isConnectSelector(index.X, connectAlias, "Response") {
		return index.Index, true
	}
	return nil, false
}

// unwrapConnectGeneric turns *connect.<typeName>[T] into *T.
func unwrapConnectGeneric(typ ast.Expr, connectAlias, typeName string) (ast.Expr, bool) {
	star, isStar := typ.(*ast.StarExpr)
	if !isStar {
		return typ, false
	}
	idx, isIndex := star.X.(*ast.IndexExpr)
	if !isIndex {
		return typ, false
	}
	sel, isSel := idx.X.(*ast.SelectorExpr)
	if !isSel {
		return typ, false
	}
	ident, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return typ, false
	}
	if ident.Name != connectAlias {
		return typ, false
	}
	if sel.Sel.Name != typeName {
		return typ, false
	}
	return &ast.StarExpr{X: idx.Index}, true
}

// rewriteFuncBody drops `.Msg` from expressions rooted at a value in msgHolders
// (server request params and client response holders), rewrites server
// request-header access to the v2 CallInfo, and warns on response-holder
// metadata access (which moves to the client CallInfo).
func rewriteFuncBody(body *ast.BlockStmt, state *rewriteState, report *Report, msgHolders, serverRequests, clientRequests, clientResponses map[string]bool, ctxName string) {
	if len(msgHolders) == 0 && len(clientRequests) == 0 {
		return
	}
	if len(msgHolders) > 0 {
		// Drop the .Msg from `x.Msg.<...>` chains.
		walkFuncBody(body, func(n ast.Node) {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return
			}
			root := rootIdent(sel)
			if root == nil || !msgHolders[root.Name] {
				return
			}
			inner, isSelector := sel.X.(*ast.SelectorExpr)
			if !isSelector || inner.Sel.Name != identMsg {
				return
			}
			innerRoot := rootIdent(inner)
			if innerRoot == nil || !msgHolders[innerRoot.Name] {
				return
			}
			sel.X = inner.X
			report.bump("body_drop_msg")
		})

		serverResponses := scanServerResponseHolders(body, state.connectAlias)
		rewriteRequestMetadata(body, state, report, serverRequests, serverResponses, ctxName)
		rewriteBareMsg(body, msgHolders, report)
	}
	// A client request header set here seeds a context; the response pass then
	// warns rather than inserting a second NewClientContext.
	requestHeaderSet := false
	if len(clientRequests) > 0 {
		requestHeaderSet = rewriteClientRequestHeaders(body, state, report, clientRequests, ctxName)
	}
	if len(clientResponses) > 0 {
		rewriteClientResponseMetadata(body, state, report, clientResponses, ctxName, requestHeaderSet)
	}
}

// rewriteRequestMetadata moves server-side metadata access to the v2 CallInfo.
// Client response info is done in rewriteClientResponseMetadata and .Spec() is
// warned.
func rewriteRequestMetadata(body *ast.BlockStmt, state *rewriteState, report *Report, serverRequests, serverResponses map[string]bool, ctxName string) {
	infoName := ""
	walkFuncBody(body, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return
		}
		root := rootIdent(sel)
		if root == nil {
			return
		}
		name := root.Name
		// .Spec() has no CallInfo equivalent; it becomes a handler argument.
		if sel.Sel.Name == "Spec" && serverRequests[name] {
			report.warnAtf(call.Pos(), ruleRequestMetadata, "%s.Spec() arrives as a separate handler argument in v2", name)
			return
		}
		// Map the v1 metadata access to its v2 CallInfo method and report key.
		var method, bumpKey, warnMsg string
		switch {
		case sel.Sel.Name == identHeader && serverRequests[name]:
			method, bumpKey = "RequestHeader", "server_header_rewrite"
			warnMsg = "%s.Header() reads request headers via the connect.CallInfoForServerContext(ctx) info's RequestHeader() in v2"
		case sel.Sel.Name == identHeader && serverResponses[name]:
			method, bumpKey = "ResponseHeader", "server_response_metadata_rewrite"
			warnMsg = "%s.Header() sets response headers via the connect.CallInfoForServerContext(ctx) info's ResponseHeader() in v2"
		case sel.Sel.Name == "Trailer" && serverResponses[name]:
			method, bumpKey = "ResponseTrailer", "server_response_metadata_rewrite"
			warnMsg = "%s.Trailer() sets response trailers via the connect.CallInfoForServerContext(ctx) info's ResponseTrailer() in v2"
		default:
			return
		}
		if ctxName == "" {
			report.warnAtf(call.Pos(), ruleRequestMetadata, warnMsg, name)
			return
		}
		if infoName == "" {
			infoName = ensureServerCallInfo(body, state, ctxName)
		}
		call.Fun = &ast.SelectorExpr{X: ast.NewIdent(infoName), Sel: ast.NewIdent(method)}
		report.bump(bumpKey)
	})
}

// ensureServerCallInfo returns the name of a server CallInfo variable usable
// from body, seeding `info, _ := connect.CallInfoForServerContext(ctx)` at the
// top of body when no earlier pass already did. CallInfoForServerContext
// returns (*CallInfo, bool), so metadata access needs a hoisted variable
// rather than an inline chained call.
func ensureServerCallInfo(body *ast.BlockStmt, state *rewriteState, ctxName string) string {
	if name, ok := serverCallInfoSeedName(body, state.connectV2Alias); ok {
		return name
	}
	name := uniqueIdent(body, "info", "callInfo")
	state.usedV2 = true
	seed := &ast.AssignStmt{
		Lhs: []ast.Expr{ast.NewIdent(name), ast.NewIdent("_")},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{&ast.CallExpr{
			Fun: &ast.SelectorExpr{
				X:   ast.NewIdent(state.connectV2Alias),
				Sel: ast.NewIdent("CallInfoForServerContext"),
			},
			Args: []ast.Expr{ast.NewIdent(ctxName)},
		}},
	}
	body.List = append([]ast.Stmt{seed}, body.List...)
	return name
}

// serverCallInfoSeedName returns the variable holding an existing top-level
// `<name>, <ok> := connect.CallInfoForServerContext(...)` seed in body.
func serverCallInfoSeedName(body *ast.BlockStmt, connectV2Alias string) (string, bool) {
	for _, stmt := range body.List {
		assign, isAssign := stmt.(*ast.AssignStmt)
		if !isAssign || assign.Tok != token.DEFINE || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 {
			continue
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "CallInfoForServerContext" {
			continue
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != connectV2Alias {
			continue
		}
		if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
			return id.Name, true
		}
	}
	return "", false
}

func contextParamName(funcType *ast.FuncType) string {
	if funcType == nil || funcType.Params == nil {
		return ""
	}
	for _, field := range funcType.Params.List {
		if len(field.Names) == 0 {
			continue
		}
		if isContextType(field.Type) {
			// A blank `_ context.Context` isn't usable as a value; treat it as
			// no context so callers warn rather than emit ...ForContext(_).
			if name := field.Names[0].Name; name != "_" {
				return name
			}
			return ""
		}
	}
	return ""
}

// receiverTypeName returns a method receiver's base type name ("Ingester" for
// (i Ingester) and (i *Ingester[T])), or "" for no receiver.
func receiverTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	if star, isStar := expr.(*ast.StarExpr); isStar {
		expr = star.X
	}
	switch typ := expr.(type) {
	case *ast.Ident:
		return typ.Name
	case *ast.IndexExpr:
		if id, isIdent := typ.X.(*ast.Ident); isIdent {
			return id.Name
		}
	case *ast.IndexListExpr:
		if id, isIdent := typ.X.(*ast.Ident); isIdent {
			return id.Name
		}
	}
	return ""
}

func isContextType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Context" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == "context"
}

// rewriteClientRequestHeaders moves a client's req.Header() writes to a
// connect.NewClientContext info. It reports whether any request header is
// mutated here, so the response pass can avoid seeding a second context.
func rewriteClientRequestHeaders(body *ast.BlockStmt, state *rewriteState, report *Report, requestVars map[string]bool, ctxName string) bool {
	// Only request holders whose headers are mutated need a client context.
	withHeaders := map[string]bool{}
	for name := range requestVars {
		if bodyContainsRequestHeader(body, map[string]bool{name: true}) {
			withHeaders[name] = true
		}
	}
	if len(withHeaders) == 0 {
		return false
	}
	// Without a usable context, or with multiple header-mutating requests, one
	// shared NewClientContext would conflate their metadata; warn instead.
	if ctxName == "" || len(withHeaders) > 1 {
		for name := range withHeaders {
			report.warnAtf(firstCallPos(body, name, identHeader), ruleRequestMetadata, "%s.Header() writes client request headers via connect.NewClientContext(ctx) and info.RequestHeader() in v2", name)
		}
		return true
	}
	infoName := uniqueIdent(body, "info", "callInfo")
	if first := firstStmtWithRequestHeader(body, withHeaders); first >= 0 {
		seed := newClientContextSeed(state, ctxName, infoName)
		body.List = append(body.List[:first], append([]ast.Stmt{seed}, body.List[first:]...)...)
		report.bump("client_context_insert")
	}
	walkFuncBody(body, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != identHeader {
			return
		}
		root := rootIdent(sel)
		if root == nil || !withHeaders[root.Name] {
			return
		}
		call.Fun = &ast.SelectorExpr{
			X:   ast.NewIdent(infoName),
			Sel: ast.NewIdent("RequestHeader"),
		}
		state.usedV2 = true
		report.bump("client_header_rewrite")
	})
	return true
}

// rewriteClientResponseMetadata moves a client's res.Header()/res.Trailer()
// reads to a connect.NewClientContext info seeded before the call. A single
// response holder with a usable context is rewritten; other shapes (no context,
// several holders, or a request header already seeding a context) are warned.
func rewriteClientResponseMetadata(body *ast.BlockStmt, state *rewriteState, report *Report, responseHolders map[string]bool, ctxName string, requestHeaderSet bool) {
	const warnMsg = "%s response metadata reads via connect.NewClientContext(ctx) and info.ResponseHeader()/ResponseTrailer() in v2"
	// withMeta maps each response holder that reads metadata to the position of
	// its first .Header()/.Trailer() call.
	withMeta := map[string]token.Pos{}
	for name := range responseHolders {
		if pos := firstCallPos(body, name, identHeader, "Trailer"); pos.IsValid() {
			withMeta[name] = pos
		}
	}
	if len(withMeta) == 0 {
		return
	}
	// A usable context, a single holder, and no request seed are required to
	// inject one NewClientContext without conflating metadata; warn otherwise.
	if ctxName == "" || len(withMeta) > 1 || requestHeaderSet {
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
		if !ok {
			return
		}
		root := rootIdent(sel)
		if root == nil || root.Name != holder {
			return
		}
		switch sel.Sel.Name {
		case identHeader:
			call.Fun = &ast.SelectorExpr{X: ast.NewIdent(infoName), Sel: ast.NewIdent("ResponseHeader")}
			report.bump("client_response_metadata_rewrite")
		case "Trailer":
			call.Fun = &ast.SelectorExpr{X: ast.NewIdent(infoName), Sel: ast.NewIdent("ResponseTrailer")}
			report.bump("client_response_metadata_rewrite")
		}
	})
}

// bodyHasNewClientContext reports whether the body already seeds a client
// context, so a later pass does not insert a second one.
func bodyHasNewClientContext(body *ast.BlockStmt, connectV2Alias string) bool {
	found := false
	walkFuncBody(body, func(n ast.Node) {
		if found {
			return
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewClientContext" {
			return
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == connectV2Alias {
			found = true
		}
	})
	return found
}

// newClientContextSeed builds `ctx, info := connect.NewClientContext(ctx)`.
func newClientContextSeed(state *rewriteState, ctxName, infoName string) *ast.AssignStmt {
	state.usedV2 = true
	return &ast.AssignStmt{
		Lhs: []ast.Expr{ast.NewIdent(ctxName), ast.NewIdent(infoName)},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{&ast.CallExpr{
			Fun:  &ast.SelectorExpr{X: ast.NewIdent(state.connectV2Alias), Sel: ast.NewIdent("NewClientContext")},
			Args: []ast.Expr{ast.NewIdent(ctxName)},
		}},
	}
}

// firstCallPos returns the position of the first name.<method>() call for one
// of methods, or token.NoPos.
func firstCallPos(body *ast.BlockStmt, name string, methods ...string) token.Pos {
	var pos token.Pos
	walkFuncBody(body, func(n ast.Node) {
		if pos.IsValid() {
			return
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !slices.Contains(methods, sel.Sel.Name) {
			return
		}
		if root := rootIdent(sel); root != nil && root.Name == name {
			pos = call.Pos()
		}
	})
	return pos
}

// holderAssignIndex returns the index in body.List of the top-level statement
// that assigns name, or -1.
func holderAssignIndex(body *ast.BlockStmt, name string) int {
	for index, stmt := range body.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok {
			continue
		}
		for _, lhs := range assign.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
				return index
			}
		}
	}
	return -1
}

func firstStmtWithRequestHeader(body *ast.BlockStmt, requestVars map[string]bool) int {
	for i, stmt := range body.List {
		if bodyContainsRequestHeader(stmt, requestVars) {
			return i
		}
	}
	return -1
}

func bodyContainsRequestHeader(node ast.Node, requestVars map[string]bool) bool {
	found := false
	walkFuncBody(node, func(n ast.Node) {
		if found {
			return
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != identHeader {
			return
		}
		root := rootIdent(sel)
		found = root != nil && requestVars[root.Name]
	})
	return found
}

func uniqueIdent(body *ast.BlockStmt, preferred ...string) string {
	used := map[string]bool{}
	walkFuncBody(body, func(n ast.Node) {
		if id, ok := n.(*ast.Ident); ok {
			used[id.Name] = true
		}
	})
	for _, name := range preferred {
		if !used[name] {
			return name
		}
	}
	for i := 2; ; i++ {
		name := fmt.Sprintf("%s%d", preferred[len(preferred)-1], i)
		if !used[name] {
			return name
		}
	}
}

// rewriteBareMsg replaces bare `x.Msg` with `x` in common expression positions,
// complementing rewriteFuncBody, which handles `x.Msg.<...>` chains.
func rewriteBareMsg(body *ast.BlockStmt, unwrapped map[string]bool, report *Report) {
	replace := func(expr *ast.Expr) {
		sel, ok := (*expr).(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != identMsg {
			return
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || !unwrapped[id.Name] {
			return
		}
		*expr = id
		report.bump("body_drop_msg")
	}
	walkFuncBody(body, func(n ast.Node) {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for i := range node.Rhs {
				replace(&node.Rhs[i])
			}
		case *ast.ReturnStmt:
			for i := range node.Results {
				replace(&node.Results[i])
			}
		case *ast.CallExpr:
			for i := range node.Args {
				replace(&node.Args[i])
			}
		case *ast.BinaryExpr:
			replace(&node.X)
			replace(&node.Y)
		case *ast.UnaryExpr:
			replace(&node.X)
		case *ast.IndexExpr:
			replace(&node.Index)
		case *ast.KeyValueExpr:
			replace(&node.Value)
		case *ast.CompositeLit:
			for i := range node.Elts {
				replace(&node.Elts[i])
			}
		case *ast.SendStmt:
			replace(&node.Value)
		}
	})
}

// rewriteFileExprs applies the context-independent expression rewrites
// (rewriteExpr) across the whole file.
//
//nolint:gocyclo // dispatch table over many AST shapes
func rewriteFileExprs(file *ast.File, state *rewriteState, report *Report) {
	walkExpr := func(exprPtr *ast.Expr) {
		rewriteExpr(exprPtr, state, report)
	}
	walk(file, func(n ast.Node) {
		switch node := n.(type) {
		case *ast.CallExpr:
			walkExpr(&node.Fun)
			for i := range node.Args {
				walkExpr(&node.Args[i])
			}
		case *ast.AssignStmt:
			for i := range node.Rhs {
				walkExpr(&node.Rhs[i])
			}
			for i := range node.Lhs {
				walkExpr(&node.Lhs[i])
			}
		case *ast.ReturnStmt:
			for i := range node.Results {
				walkExpr(&node.Results[i])
			}
		case *ast.ValueSpec:
			for i := range node.Values {
				walkExpr(&node.Values[i])
			}
			if node.Type != nil {
				walkExpr(&node.Type)
			}
		case *ast.Field:
			walkExpr(&node.Type)
		case *ast.KeyValueExpr:
			walkExpr(&node.Key)
			walkExpr(&node.Value)
		case *ast.CompositeLit:
			if node.Type != nil {
				walkExpr(&node.Type)
			}
			for i := range node.Elts {
				walkExpr(&node.Elts[i])
			}
		case *ast.IndexExpr:
			walkExpr(&node.X)
			walkExpr(&node.Index)
		case *ast.IndexListExpr:
			walkExpr(&node.X)
			for i := range node.Indices {
				walkExpr(&node.Indices[i])
			}
		case *ast.StarExpr:
			walkExpr(&node.X)
		case *ast.UnaryExpr:
			walkExpr(&node.X)
		case *ast.BinaryExpr:
			walkExpr(&node.X)
			walkExpr(&node.Y)
		case *ast.ParenExpr:
			walkExpr(&node.X)
		case *ast.SwitchStmt:
			if node.Tag != nil {
				walkExpr(&node.Tag)
			}
		case *ast.CaseClause:
			for i := range node.List {
				walkExpr(&node.List[i])
			}
		case *ast.IfStmt:
			if node.Cond != nil {
				walkExpr(&node.Cond)
			}
		case *ast.ForStmt:
			if node.Cond != nil {
				walkExpr(&node.Cond)
			}
		case *ast.ExprStmt:
			walkExpr(&node.X)
		case *ast.SendStmt:
			walkExpr(&node.Chan)
			walkExpr(&node.Value)
		}
	})
}

// rewriteProtocolOption flips connect.WithGRPC()/WithGRPCWeb() to the
// connecthttp package (the name is unchanged in v2), reporting whether it matched.
func rewriteProtocolOption(expr ast.Expr, state *rewriteState, report *Report) bool {
	call, isCall := expr.(*ast.CallExpr)
	if !isCall || len(call.Args) != 0 {
		return false
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return false
	}
	pkg, isIdent := sel.X.(*ast.Ident)
	if !isIdent || pkg.Name != state.connectAlias {
		return false
	}
	if _, ok := connectProtocolOptions[sel.Sel.Name]; !ok {
		return false
	}
	pkg.Name = state.connectHTTPAlias
	state.usedConnectHTTP = true
	report.bump("option_protocol")
	return true
}

func rewriteExpr(exprPtr *ast.Expr, state *rewriteState, report *Report) {
	expr := *exprPtr

	// connect.NewResponse(x) / connect.NewRequest(x) -> x (v2 passes the bare
	// message). Stub-dependent, so gated on stubsReady.
	if call, ok := expr.(*ast.CallExpr); ok && state.stubsReady && len(call.Args) == 1 {
		if isConnectSelector(call.Fun, state.connectAlias, "NewResponse") {
			*exprPtr = call.Args[0]
			report.bump("strip_new_response")
			return
		}
		if isConnectSelector(call.Fun, state.connectAlias, "NewRequest") {
			*exprPtr = call.Args[0]
			report.bump("strip_new_request")
			return
		}
	}

	// Struct-literal wrapper form: &connect.Response[T]{Msg: x} -> x. Stub-dependent.
	if state.stubsReady {
		if msg, rule, ok := unwrapConnectMessageLiteral(expr, state.connectAlias); ok {
			*exprPtr = msg
			report.bump(rule)
			return
		}
	}

	if rewriteProtocolOption(expr, state, report) {
		return
	}

	// connect.NewError(c, err); see convertNewErrorInPlace for the cases.
	if call, ok := expr.(*ast.CallExpr); ok && len(call.Args) == 2 && isConnectSelector(call.Fun, state.connectAlias, "NewError") {
		if convertNewErrorInPlace(call, state) {
			state.usedV2 = true
			report.bump("convert_new_error")
		}
		return
	}

	// Qualifier-only flips (Errorf, CodeOf, Code<X>, Error type).
	if call, ok := expr.(*ast.CallExpr); ok && isConnectSelector(call.Fun, state.connectAlias, identErrorf) {
		rewriteSelectorPkg(call.Fun, state)
		state.usedV2 = true
		report.bump("convert_errorf")
		return
	}
	if call, ok := expr.(*ast.CallExpr); ok && isConnectSelector(call.Fun, state.connectAlias, "CodeOf") {
		rewriteSelectorPkg(call.Fun, state)
		state.usedV2 = true
		report.bump("convert_code_of")
		return
	}
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		if isConnectIdent(sel.X, state.connectAlias) && strings.HasPrefix(sel.Sel.Name, "Code") {
			rewriteSelectorPkg(expr, state)
			state.usedV2 = true
			report.bump("convert_code_const")
			return
		}
		if isConnectIdent(sel.X, state.connectAlias) && sel.Sel.Name == identError {
			rewriteSelectorPkg(expr, state)
			state.usedV2 = true
			report.bump("convert_error_type")
			return
		}
	}
}

// convertNewErrorInPlace mutates a v1 connect.NewError(code, err) call to v2:
//
//   - errors.New("s")        -> connect.NewError(code, "s")
//   - fmt.Errorf(f, args...) -> connect.Errorf(code, f, args...)
//   - fmt.Errorf(... %w ...) -> connect.NewError(code, fmt.Errorf(...).Error())
//   - nil                    -> connect.NewError(code, "")
//   - other err expr         -> connect.NewError(code, err.Error())
//
// %w and the fallback keep err.Error() on the wire (matching v1; WithCause would
// hide it). An already-string message arg is left alone (false), so the rewrite
// is idempotent.
func convertNewErrorInPlace(call *ast.CallExpr, state *rewriteState) bool {
	errArg := call.Args[1]
	if isMessageStringExpr(errArg) {
		return false
	}
	// The remaining branches migrate a v1 argument, so flip the qualifier to v2.
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok {
			id.Name = state.connectV2Alias
		}
	}
	if id, ok := errArg.(*ast.Ident); ok && id.Name == identNil {
		// v1 allowed a nil error for a code-only error; v2 needs a string.
		pos := errArg.Pos()
		call.Args[1] = &ast.BasicLit{ValuePos: pos, Kind: token.STRING, Value: `""`}
		return true
	}
	if inner, ok := errArg.(*ast.CallExpr); ok && rewriteNewErrorCallArg(call, inner) {
		return true
	}
	// Fallback: connect.NewError(code, err.Error()), anchored to errArg's position.
	pos := errArg.Pos()
	call.Args[1] = &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X:   errArg,
			Sel: &ast.Ident{NamePos: pos, Name: identError},
		},
		Lparen: pos,
		Rparen: pos,
	}
	return true
}

// isMessageStringExpr reports whether expr is already a v2 message string: a
// string literal or a zero-arg x.Error() call.
func isMessageStringExpr(expr ast.Expr) bool {
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		return true
	}
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == identError
}

// rewriteNewErrorCallArg handles a v1 connect.NewError whose error argument is
// itself errors.New or fmt.Errorf, mutating call in place. False leaves it for
// the caller's .Error() fallback.
func rewriteNewErrorCallArg(call, inner *ast.CallExpr) bool {
	if len(inner.Args) == 1 && isErrorsNew(inner.Fun) {
		call.Args[1] = inner.Args[0]
		return true
	}
	if !isFmtErrorf(inner.Fun) {
		return false
	}
	if containsErrorWrap(inner.Args) {
		// v2's Errorf is plain Sprintf (%w is literal); splitting into WithCause
		// would drop the cause from the wire. Use the .Error() fallback instead.
		return false
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		sel.Sel.Name = identErrorf
	}
	newArgs := make([]ast.Expr, 0, 1+len(inner.Args))
	newArgs = append(newArgs, call.Args[0])
	newArgs = append(newArgs, inner.Args...)
	call.Args = newArgs
	return true
}

func isErrorsNew(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == "errors" && sel.Sel.Name == "New"
}

func isFmtErrorf(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == "fmt" && sel.Sel.Name == identErrorf
}

// containsErrorWrap reports whether an fmt.Errorf format string contains %w. A
// non-literal format conservatively returns true (forcing the .Error() path).
func containsErrorWrap(args []ast.Expr) bool {
	if len(args) == 0 {
		return false
	}
	lit, ok := args[0].(*ast.BasicLit)
	if !ok {
		return true
	}
	return strings.Contains(lit.Value, "%w")
}

// unwrapConnectMessageLiteral returns the Msg value of a &connect.Request/Response[T]{Msg: x}
// literal so the wrapper can be replaced by the bare message.
func unwrapConnectMessageLiteral(expr ast.Expr, connectAlias string) (msg ast.Expr, rule string, ok bool) {
	unary, isUnary := expr.(*ast.UnaryExpr)
	if !isUnary || unary.Op != token.AND {
		return nil, "", false
	}
	lit, isLit := unary.X.(*ast.CompositeLit)
	if !isLit {
		return nil, "", false
	}
	index, isIndex := lit.Type.(*ast.IndexExpr)
	if !isIndex {
		return nil, "", false
	}
	switch {
	case isConnectSelector(index.X, connectAlias, "Response"):
		rule = "strip_response_literal"
	case isConnectSelector(index.X, connectAlias, "Request"):
		rule = "strip_request_literal"
	default:
		return nil, "", false
	}
	for _, elt := range lit.Elts {
		keyValue, isKeyValue := elt.(*ast.KeyValueExpr)
		if !isKeyValue {
			continue
		}
		if key, isIdent := keyValue.Key.(*ast.Ident); isIdent && key.Name == "Msg" {
			return keyValue.Value, rule, true
		}
	}
	// No Msg field (&connect.Response[T]{}): flip the wrapper type to give &T{}.
	lit.Type = index.Index
	return expr, rule, true
}

func isConnectSelector(fun ast.Expr, connectAlias, name string) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return isConnectIdent(sel.X, connectAlias) && sel.Sel.Name == name
}

func isConnectIdent(expr ast.Expr, connectAlias string) bool {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == connectAlias
}

// rewriteSelectorPkg flips a selector's v1 connect qualifier to v2, accepting a
// SelectorExpr or a CallExpr whose Fun is one.
func rewriteSelectorPkg(expr ast.Expr, state *rewriteState) {
	var sel *ast.SelectorExpr
	switch node := expr.(type) {
	case *ast.SelectorExpr:
		sel = node
	case *ast.CallExpr:
		funSel, isSel := node.Fun.(*ast.SelectorExpr)
		if !isSel {
			return
		}
		sel = funSel
	default:
		return
	}
	ident, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return
	}
	if ident.Name != state.connectAlias {
		return
	}
	ident.Name = state.connectV2Alias
}

// ensureConnectV2Import adds the connectrpc.com/connect/v2 import if absent.
func ensureConnectV2Import(fset *token.FileSet, file *ast.File, state *rewriteState) {
	if state.hadV2Import {
		return
	}
	if state.connectV2Alias == "connect" {
		astutil.AddImport(fset, file, "connectrpc.com/connect/v2")
	} else {
		astutil.AddNamedImport(fset, file, state.connectV2Alias, "connectrpc.com/connect/v2")
	}
	state.hadV2Import = true
}

// ensureConnectHTTPImport adds the connecthttp import if absent.
func ensureConnectHTTPImport(fset *token.FileSet, file *ast.File, state *rewriteState) {
	if state.hadConnectHTTP {
		return
	}
	if state.connectHTTPAlias == "connecthttp" {
		astutil.AddImport(fset, file, "connectrpc.com/connect/v2/connecthttp")
	} else {
		astutil.AddNamedImport(fset, file, state.connectHTTPAlias, "connectrpc.com/connect/v2/connecthttp")
	}
	state.hadConnectHTTP = true
}

func removeConnectV1Import(fset *token.FileSet, file *ast.File, state *rewriteState) {
	astutil.DeleteImport(fset, file, "connectrpc.com/connect")
	state.hadV1Import = false
}

// fileReferencesIdent reports whether alias is still used as a selector
// qualifier anywhere in the file.
func fileReferencesIdent(file *ast.File, alias string) bool {
	found := false
	walk(file, func(n ast.Node) {
		if found {
			return
		}
		sel, isSel := n.(*ast.SelectorExpr)
		if !isSel {
			return
		}
		ident, isIdent := sel.X.(*ast.Ident)
		if !isIdent {
			return
		}
		if ident.Name == alias {
			found = true
		}
	})
	return found
}

// residualConnectSymbols returns the sorted `connect.<X>` selectors still under
// the v1 alias (the warned-only symbols that kept the v1 import alive).
func residualConnectSymbols(file *ast.File, alias string) []string {
	seen := map[string]bool{}
	walk(file, func(n ast.Node) {
		sel, isSel := n.(*ast.SelectorExpr)
		if !isSel {
			return
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == alias {
			seen[alias+"."+sel.Sel.Name] = true
		}
	})
	symbols := make([]string, 0, len(seen))
	for symbol := range seen {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	return symbols
}

// flipRemainingConnectSelectors flips the v1->v2 qualifier on the connect
// symbols that need only that, anywhere the position-specific passes missed:
// Code/Code<X>/CodeOf, Error, Errorf, and Encode/DecodeBinaryHeader. Symbols
// that need restructuring (Request/Response/NewError/...) are handled earlier.
func flipRemainingConnectSelectors(file *ast.File, state *rewriteState, report *Report) {
	walk(file, func(n ast.Node) {
		sel, isSel := n.(*ast.SelectorExpr)
		if !isSel {
			return
		}
		ident, isIdent := sel.X.(*ast.Ident)
		if !isIdent || ident.Name != state.connectAlias {
			return
		}
		switch {
		case strings.HasPrefix(sel.Sel.Name, "Code"): // Code, CodeOf, Code<X>
		case sel.Sel.Name == identError || sel.Sel.Name == identErrorf:
		case sel.Sel.Name == "EncodeBinaryHeader" || sel.Sel.Name == "DecodeBinaryHeader":
		default:
			return
		}
		ident.Name = state.connectV2Alias
		state.usedV2 = true
		report.bump("flip_residual_connect")
	})
}

// rewriteMovedSymbols handles the package split: relocated options flip to
// connecthttp, reshaped ones become warnings, core renames keep the connect
// qualifier.
func rewriteMovedSymbols(file *ast.File, state *rewriteState, report *Report) {
	warnedReshaped := map[string]bool{}
	walk(file, func(n ast.Node) {
		sel, isSel := n.(*ast.SelectorExpr)
		if !isSel {
			return
		}
		ident, isIdent := sel.X.(*ast.Ident)
		if !isIdent || ident.Name != state.connectAlias {
			return
		}
		name := sel.Sel.Name
		switch {
		case renamedConnectCore[name] != "":
			sel.Sel.Name = renamedConnectCore[name]
			state.usedV2 = true
			report.bump("rename_connect_core")
		case movedToConnectHTTP[name]:
			ident.Name = state.connectHTTPAlias
			state.usedConnectHTTP = true
			report.bump("option_to_connecthttp")
		case reshapedToConnectHTTP[name] != "":
			if !warnedReshaped[name] {
				report.warnAtf(sel.Pos(), ruleConnectHTTPOption, "connect.%s -> %s", name, reshapedToConnectHTTP[name])
				warnedReshaped[name] = true
			}
		case reshapedErrorAPI[name] != "":
			if !warnedReshaped[name] {
				report.warnAtf(sel.Pos(), ruleErrorAPI, "connect.%s -> %s", name, reshapedErrorAPI[name])
				warnedReshaped[name] = true
			}
		case reshapedConstruction[name] != "":
			if !warnedReshaped[name] {
				report.warnAtf(sel.Pos(), ruleServerInterceptor, "connect.%s -> %s", name, reshapedConstruction[name])
				warnedReshaped[name] = true
			}
		}
	})
}

// renameHTTPOnlyOptionTypes renames a helper's []connect.XOption result type
// (and the literals it returns) to []connecthttp.Option, but only when every
// element is a recognized HTTP option. A set carrying connect.WithInterceptors
// or anything unrecognized is left for the reshaped-construction warning. It
// runs before the element-flipping passes, so it matches the v1 element names.
func renameHTTPOnlyOptionTypes(file *ast.File, state *rewriteState, report *Report) {
	walk(file, func(n ast.Node) {
		funcType, body := funcTypeAndBody(n)
		if funcType == nil || body == nil {
			return
		}
		resultSel := soleOptionSliceResult(funcType, state.connectAlias)
		if resultSel == nil {
			return
		}
		lits, ok := returnedOptionLiterals(body, state.connectAlias)
		if !ok || len(lits) == 0 {
			return
		}
		for _, lit := range lits {
			if !optionLiteralHTTPOnly(lit, state.connectAlias) {
				return
			}
		}
		renameOptionTypeSelector(resultSel, state)
		for _, lit := range lits {
			if litSel := optionSliceEltSelector(lit.Type, state.connectAlias); litSel != nil {
				renameOptionTypeSelector(litSel, state)
			}
		}
		state.usedConnectHTTP = true
		report.bump("option_type_to_connecthttp")
	})
}

// funcTypeAndBody returns the signature and body of a FuncDecl or FuncLit.
func funcTypeAndBody(n ast.Node) (*ast.FuncType, *ast.BlockStmt) {
	switch node := n.(type) {
	case *ast.FuncDecl:
		return node.Type, node.Body
	case *ast.FuncLit:
		return node.Type, node.Body
	}
	return nil, nil
}

// soleOptionSliceResult returns the connect.XOption selector of a function
// whose only result is a []connect.XOption slice, or nil.
func soleOptionSliceResult(funcType *ast.FuncType, connectAlias string) *ast.SelectorExpr {
	if funcType.Results == nil || len(funcType.Results.List) != 1 {
		return nil
	}
	field := funcType.Results.List[0]
	if len(field.Names) > 1 {
		return nil
	}
	return optionSliceEltSelector(field.Type, connectAlias)
}

// optionSliceEltSelector returns the connect.XOption selector of a
// []connect.XOption type expression, or nil.
func optionSliceEltSelector(typ ast.Expr, connectAlias string) *ast.SelectorExpr {
	arr, isArray := typ.(*ast.ArrayType)
	if !isArray || arr.Len != nil {
		return nil
	}
	sel, isSel := arr.Elt.(*ast.SelectorExpr)
	if !isSel {
		return nil
	}
	id, isIdent := sel.X.(*ast.Ident)
	if !isIdent || id.Name != connectAlias || !optionTypeNames[sel.Sel.Name] {
		return nil
	}
	return sel
}

// returnedOptionLiterals collects the []connect.XOption literals a helper
// returns. ok is false if any return yields something else (variable, call,
// append), whose elements can't be proven HTTP-only.
func returnedOptionLiterals(body *ast.BlockStmt, connectAlias string) (lits []*ast.CompositeLit, ok bool) {
	ok = true
	walkFuncBody(body, func(n ast.Node) {
		ret, isRet := n.(*ast.ReturnStmt)
		if !isRet {
			return
		}
		if len(ret.Results) != 1 {
			ok = false
			return
		}
		switch result := ret.Results[0].(type) {
		case *ast.CompositeLit:
			if optionSliceEltSelector(result.Type, connectAlias) == nil {
				ok = false
				return
			}
			lits = append(lits, result)
		case *ast.Ident:
			if result.Name != identNil {
				ok = false
			}
		default:
			ok = false
		}
	})
	return lits, ok
}

// optionLiteralHTTPOnly reports whether every element of the literal is a
// recognized HTTP option (an empty literal qualifies).
func optionLiteralHTTPOnly(lit *ast.CompositeLit, connectAlias string) bool {
	for _, elt := range lit.Elts {
		if !isHTTPOnlyOptionElement(elt, connectAlias) {
			return false
		}
	}
	return true
}

// isHTTPOnlyOptionElement reports whether elt is a call to a v1 connect option
// that moves, renames, or is a protocol selector. WithInterceptors, custom
// options, and bare variables are not recognized.
func isHTTPOnlyOptionElement(elt ast.Expr, connectAlias string) bool {
	call, isCall := elt.(*ast.CallExpr)
	if !isCall {
		return false
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return false
	}
	id, isIdent := sel.X.(*ast.Ident)
	if !isIdent || id.Name != connectAlias {
		return false
	}
	name := sel.Sel.Name
	return movedToConnectHTTP[name] || connectProtocolOptions[name] != ""
}

// renameOptionTypeSelector flips a connect.XOption type selector to
// connecthttp.Option in place.
func renameOptionTypeSelector(sel *ast.SelectorExpr, state *rewriteState) {
	if id, isIdent := sel.X.(*ast.Ident); isIdent {
		id.Name = state.connectHTTPAlias
	}
	sel.Sel.Name = "Option"
}

// scanClientResponseHolders finds locals holding a v1 client call result
// (resp, err := client.Foo(ctx, connect.NewRequest(req))). In v2 the call
// returns the bare response, so resp loses its .Msg.
func scanClientResponseHolders(body *ast.BlockStmt, connectAlias string, requestHolders map[string]bool) map[string]bool {
	holders := map[string]bool{}
	walkFuncBody(body, func(n ast.Node) {
		assign, isAssign := n.(*ast.AssignStmt)
		if !isAssign || len(assign.Lhs) == 0 || len(assign.Rhs) == 0 {
			return
		}
		call, isCall := assign.Rhs[0].(*ast.CallExpr)
		if !isCall {
			return
		}
		if !callContainsConnectNewRequest(call, connectAlias, requestHolders) {
			return
		}
		first, isIdent := assign.Lhs[0].(*ast.Ident)
		if !isIdent || first.Name == "_" {
			return
		}
		holders[first.Name] = true
	})
	return holders
}

// scanClientRequestHolders finds locals initialized from connect.NewRequest, so
// later req.Header() calls on them can be rewritten to the v2 CallInfo.
// scanServerResponseHolders returns the names of variables bound to a
// connect.NewResponse(...) result. NewResponse is server-only, so these hold
// server response metadata; their .Header()/.Trailer() move to the server
// CallInfo.
func scanServerResponseHolders(body *ast.BlockStmt, connectAlias string) map[string]bool {
	holders := map[string]bool{}
	bind := func(names []ast.Expr, values []ast.Expr) {
		for i, value := range values {
			call, isCall := value.(*ast.CallExpr)
			if !isCall || !isConnectSelector(call.Fun, connectAlias, "NewResponse") || i >= len(names) {
				continue
			}
			if id, ok := names[i].(*ast.Ident); ok && id.Name != "_" {
				holders[id.Name] = true
			}
		}
	}
	walkFuncBody(body, func(n ast.Node) {
		switch node := n.(type) {
		case *ast.AssignStmt:
			bind(node.Lhs, node.Rhs)
		case *ast.ValueSpec:
			idents := make([]ast.Expr, len(node.Names))
			for i, name := range node.Names {
				idents[i] = name
			}
			bind(idents, node.Values)
		}
	})
	return holders
}

func scanClientRequestHolders(body *ast.BlockStmt, connectAlias string) map[string]bool {
	holders := map[string]bool{}
	walkFuncBody(body, func(n ast.Node) {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range node.Rhs {
				if !isConnectNewRequestCall(rhs, connectAlias) || i >= len(node.Lhs) {
					continue
				}
				if id, ok := node.Lhs[i].(*ast.Ident); ok && id.Name != "_" {
					holders[id.Name] = true
				}
			}
		case *ast.ValueSpec:
			for i, rhs := range node.Values {
				if !isConnectNewRequestCall(rhs, connectAlias) || i >= len(node.Names) {
					continue
				}
				if name := node.Names[i].Name; name != "_" {
					holders[name] = true
				}
			}
		}
	})
	return holders
}

func isConnectNewRequestCall(expr ast.Expr, connectAlias string) bool {
	call, ok := expr.(*ast.CallExpr)
	return ok && isConnectSelector(call.Fun, connectAlias, "NewRequest")
}

func callContainsConnectNewRequest(call *ast.CallExpr, connectAlias string, requestHolders map[string]bool) bool {
	_, isMethodCall := call.Fun.(*ast.SelectorExpr)
	for i, arg := range call.Args {
		if inner, ok := arg.(*ast.CallExpr); ok && isConnectSelector(inner.Fun, connectAlias, "NewRequest") {
			return true
		}
		// A request holder in a non-first method-call argument (client.Method(ctx,
		// req)) signals a client RPC; the position/method check avoids matching
		// local helpers like buildWrapper(req).
		if id, ok := arg.(*ast.Ident); ok && isMethodCall && i > 0 && requestHolders[id.Name] {
			return true
		}
	}
	return false
}

// rootIdent returns the leftmost Ident in a selector chain, or nil.
func rootIdent(expr ast.Expr) *ast.Ident {
	for {
		switch x := expr.(type) {
		case *ast.Ident:
			return x
		case *ast.SelectorExpr:
			expr = x.X
		default:
			return nil
		}
	}
}

// walk is ast.Inspect for visitors that never prune.
func walk(node ast.Node, fn func(ast.Node)) {
	ast.Inspect(node, func(n ast.Node) bool {
		if n != nil {
			fn(n)
		}
		return true
	})
}

// mergeUnwrappedScopes unions local with outer, dropping names shadowed by
// funcType's parameters. The result is safe to mutate; outer is not.
func mergeUnwrappedScopes(outer map[string]bool, funcType *ast.FuncType, local map[string]bool) map[string]bool {
	merged := map[string]bool{}
	for name := range local {
		merged[name] = true
	}
	if len(outer) == 0 {
		return merged
	}
	shadow := map[string]bool{}
	if funcType != nil && funcType.Params != nil {
		for _, field := range funcType.Params.List {
			for _, name := range field.Names {
				shadow[name.Name] = true
			}
		}
	}
	for name := range outer {
		if shadow[name] {
			continue
		}
		merged[name] = true
	}
	return merged
}

// walkFuncBody is like [walk] but does not descend into nested [*ast.FuncLit]
// bodies; the visitor still sees each FuncLit itself.
func walkFuncBody(node ast.Node, visit func(ast.Node)) {
	ast.Inspect(node, func(cur ast.Node) bool {
		if cur == nil {
			return true
		}
		if _, isLit := cur.(*ast.FuncLit); isLit && cur != node {
			visit(cur)
			return false
		}
		visit(cur)
		return true
	})
}
