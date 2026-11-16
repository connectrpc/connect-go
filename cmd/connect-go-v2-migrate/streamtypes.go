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
	"go/types"
	"strings"
)

// streamKind is the type checker's verdict on a variable that stream detection
// would otherwise judge by name. The zero value means no verdict. Higher kinds
// win when the package and test variants of a file disagree.
type streamKind uint8

const (
	// streamNone is a variable known not to be a connect stream.
	streamNone streamKind = iota + 1
	// streamV1Client is a variable of a v1 client stream type that no v2 stub
	// produced, such as a helper parameter. The tool warns at its declaration
	// and leaves its stream calls alone.
	streamV1Client
	// streamWrapper is a variable of the user's own type that holds a v1
	// connect stream, by embedding it or as an interface. The tool warns at its
	// declaration and leaves its stream calls alone.
	streamWrapper
	// streamValue is a connect stream.
	streamValue
	// streamConstructed is a stream bound from a v1-shaped constructor call
	// that now also returns an error.
	streamConstructed
)

var (
	// streamMethodList holds streamMethodNames in a fixed order.
	streamMethodList = []string{"CloseAndReceive", "CloseRequest", "CloseResponse", "CloseSend", identReceive, identSend}
	// v1StreamTypes are the concrete v1 stream types. The v1 interfaces that
	// share their method names, like StreamingClientConn, are not streams.
	v1StreamTypes = map[string]bool{
		"BidiStream":            true,
		"BidiStreamForClient":   true,
		"ClientStream":          true,
		"ClientStreamForClient": true,
		"ServerStream":          true,
		"ServerStreamForClient": true,
	}
)

// streamKinds classifies the variables declared in file, keyed by the byte
// offset of every identifier that refers to one. The warn-only kinds mark the
// declaring identifier alone, and other references get streamNone.
//
// A variable whose type the checker could not resolve is a stream when it is
// bound from a stub constructor whose result count no longer matches, like a
// v1 `stream := client.CumSum(ctx)` against v2 stubs. Any other unresolved
// variable has no verdict.
func streamKinds(file *ast.File, fset *token.FileSet, info *types.Info, genPkgs map[string]bool) map[int]streamKind {
	tokenFile := fset.File(file.Pos())
	if info == nil || tokenFile == nil {
		return nil
	}
	assigned := assignedStreamTypes(file, info, genPkgs)
	params := paramVars(file, info)
	verdicts := map[*types.Var]streamKind{}
	offsets := map[int]streamKind{}
	ast.Inspect(file, func(node ast.Node) bool {
		ident, isIdent := node.(*ast.Ident)
		if !isIdent {
			return true
		}
		variable, isVar := info.ObjectOf(ident).(*types.Var)
		if !isVar || variable.IsField() || variable.Pos() < file.FileStart || variable.Pos() >= file.FileEnd {
			return true
		}
		kind, seen := verdicts[variable]
		if !seen {
			kind = classifyStreamVar(variable, assigned[variable], params[variable], genPkgs)
			verdicts[variable] = kind
		}
		if kind == 0 {
			return true
		}
		if (kind == streamV1Client || kind == streamWrapper) && ident.Pos() != variable.Pos() {
			kind = streamNone
		}
		offsets[tokenFile.Offset(ident.Pos())] = kind
		return true
	})
	return offsets
}

// mergeStreamKinds folds one variant's verdicts for the file at path into
// byPath, keeping the higher kind for each offset.
func mergeStreamKinds(byPath map[string]map[int]streamKind, path string, kinds map[int]streamKind) {
	if kinds == nil {
		return
	}
	merged := byPath[path]
	if merged == nil {
		merged = map[int]streamKind{}
		byPath[path] = merged
	}
	for offset, kind := range kinds {
		merged[offset] = max(merged[offset], kind)
	}
}

// classifyStreamVar returns the verdict for variable. assigned is the stream
// type of a v1-shaped value assigned to it, if any.
func classifyStreamVar(variable *types.Var, assigned types.Type, isParam bool, genPkgs map[string]bool) streamKind {
	typ := variable.Type()
	if basic, isBasic := typ.Underlying().(*types.Basic); isBasic && basic.Kind() == types.Invalid {
		if assigned != nil {
			return streamConstructed
		}
		return 0
	}
	method, promoted := streamMethod(typ, genPkgs)
	switch {
	case method == nil && assigned != nil && hasStreamMethodName(typ):
		// An interface of the user's own that a stream was assigned to.
		return streamWrapper
	case method == nil:
		return streamNone
	case promoted && method.Pkg().Path() == connectV1Path:
		return streamWrapper
	case promoted:
		return streamNone
	case isV1ClientStream(typ):
		return streamV1Client
	case method.Pkg().Path() == connectV1Path && !isParam:
		// A v1 handler stream outside a handler signature, which name-based
		// detection never treated as a stream either.
		return streamNone
	}
	return streamValue
}

// streamMethod returns typ's first stream method that belongs to a concrete
// v1 stream type or a generated stub type, and whether it is promoted from an
// embedded field. It returns nil if there is none.
func streamMethod(typ types.Type, genPkgs map[string]bool) (*types.Func, bool) {
	for _, name := range streamMethodList {
		obj, index, _ := types.LookupFieldOrMethod(typ, true, nil, name)
		method, isFunc := obj.(*types.Func)
		if !isFunc || method.Pkg() == nil {
			continue
		}
		switch path := method.Pkg().Path(); {
		case path == connectV1Path && !v1StreamTypes[receiverName(method)]:
			continue
		case path == connectV2Module || !genPkgs[path]:
			continue
		}
		return method, len(index) > 1
	}
	return nil, false
}

// receiverName returns the name of method's receiver base type, or "".
func receiverName(method *types.Func) string {
	signature, isSignature := method.Type().(*types.Signature)
	if !isSignature || signature.Recv() == nil {
		return ""
	}
	named := namedType(signature.Recv().Type())
	if named == nil {
		return ""
	}
	return named.Obj().Name()
}

// hasStreamMethodName reports whether typ has a method named like a stream
// method, whatever package declares it.
func hasStreamMethodName(typ types.Type) bool {
	for _, name := range streamMethodList {
		if obj, _, _ := types.LookupFieldOrMethod(typ, true, nil, name); obj != nil {
			return true
		}
	}
	return false
}

// isV1ClientStream reports whether typ is a v1 *connect.XxxStreamForClient.
func isV1ClientStream(typ types.Type) bool {
	named := namedType(typ)
	return named != nil && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == connectV1Path &&
		strings.HasSuffix(named.Obj().Name(), "ForClient")
}

// namedType returns the named type of typ, or of the type typ points to,
// looking through aliases. It returns nil if there is none.
func namedType(typ types.Type) *types.Named {
	typ = types.Unalias(typ)
	if pointer, isPointer := typ.(*types.Pointer); isPointer {
		typ = types.Unalias(pointer.Elem())
	}
	named, _ := typ.(*types.Named)
	return named
}

// assignedStreamTypes maps each variable to the first stream type assigned to
// it in v1 shape. That is a v1 stream, or a stub constructor whose second
// result, the error, has no variable to go to. The type is read from the
// right-hand side, so it is found even when the assignment does not
// type-check.
func assignedStreamTypes(file *ast.File, info *types.Info, genPkgs map[string]bool) map[*types.Var]types.Type {
	assigned := map[*types.Var]types.Type{}
	record := func(lhs []*ast.Ident, rhs []ast.Expr) {
		for index, ident := range lhs {
			variable, isVar := info.ObjectOf(ident).(*types.Var)
			if !isVar || assigned[variable] != nil {
				continue
			}
			typ, dropsResult := rhsType(info, rhs, len(lhs), index)
			if typ == nil {
				continue
			}
			method, promoted := streamMethod(typ, genPkgs)
			if method != nil && !promoted && (dropsResult || method.Pkg().Path() == connectV1Path) {
				assigned[variable] = typ
			}
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.AssignStmt:
			lhs := make([]*ast.Ident, 0, len(node.Lhs))
			for _, expr := range node.Lhs {
				ident, isIdent := expr.(*ast.Ident)
				if !isIdent {
					// Keep indexes aligned with the right-hand side.
					ident = ast.NewIdent("_")
				}
				lhs = append(lhs, ident)
			}
			record(lhs, node.Rhs)
		case *ast.ValueSpec:
			record(node.Names, node.Values)
		}
		return true
	})
	return assigned
}

// rhsType returns the type of the value assigned to the index'th of lhsCount
// left-hand operands, or nil. dropsResult reports a call with several results
// used as a single value, which is a v1 constructor bound against v2 stubs.
// Its first result is the value's type.
func rhsType(info *types.Info, rhs []ast.Expr, lhsCount, index int) (typ types.Type, dropsResult bool) {
	switch {
	case len(rhs) == lhsCount:
		typ := info.TypeOf(rhs[index])
		if tuple, isTuple := typ.(*types.Tuple); isTuple {
			if tuple.Len() == 0 {
				return nil, false
			}
			return tuple.At(0).Type(), tuple.Len() > 1
		}
		return typ, false
	case len(rhs) == 1:
		tuple, isTuple := info.TypeOf(rhs[0]).(*types.Tuple)
		if !isTuple || tuple.Len() != lhsCount {
			return nil, false
		}
		return tuple.At(index).Type(), false
	}
	return nil, false
}

// paramVars returns the parameters of the functions and closures declared in
// file.
func paramVars(file *ast.File, info *types.Info) map[*types.Var]bool {
	params := map[*types.Var]bool{}
	addParams := func(funcType *ast.FuncType) {
		if funcType.Params == nil {
			return
		}
		for _, field := range funcType.Params.List {
			for _, name := range field.Names {
				if variable, isVar := info.Defs[name].(*types.Var); isVar {
					params[variable] = true
				}
			}
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncDecl:
			addParams(node.Type)
		case *ast.FuncLit:
			addParams(node.Type)
		}
		return true
	})
	return params
}
