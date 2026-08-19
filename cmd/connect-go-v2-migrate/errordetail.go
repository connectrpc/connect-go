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
)

// rewriteErrorDetails retargets the v1 NewErrorDetail/AddDetail guard to the
// v2 API:
//
//	if detail, derr := connect.NewErrorDetail(info); derr == nil { cErr.AddDetail(detail) }
//	-> if detail, derr := connectproto.NewErrorDetail(info); derr == nil { cErr = cErr.WithDetail(detail) }
//
// Only this tightly-coupled guard form is safe to rewrite; other
// NewErrorDetail/AddDetail uses are left for rewriteMovedSymbols to warn about.
func rewriteErrorDetails(file *ast.File, state *rewriteState, report *Report) {
	walk(file, func(n ast.Node) {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return
		}
		guard, ok := matchDetailGuard(ifStmt, state.connectAlias)
		if !ok {
			return
		}
		guard.constructorPkg.Name = "connectproto"
		state.addImport("connectrpc.com/connect/v2/connectproto", "connectproto")
		detail := &ast.Ident{Name: guard.detailName}
		ifStmt.Body.List[0] = withDetailAssign(guard.target, detail, ifStmt.Body.List[0].Pos())
		report.bump("retarget_error_detail")
	})
}

// detailGuard holds the pieces of a matched NewErrorDetail/AddDetail guard.
type detailGuard struct {
	constructorPkg *ast.Ident // the connect package ident of NewErrorDetail
	target         *ast.Ident // the error value receiving AddDetail
	detailName     string     // the detail variable bound by the guard
}

// matchDetailGuard recognises `if d, e := connect.NewErrorDetail(msg); e == nil
// { target.AddDetail(d) }`.
func matchDetailGuard(ifStmt *ast.IfStmt, connectAlias string) (detailGuard, bool) {
	var guard detailGuard
	if ifStmt.Else != nil {
		return guard, false
	}
	init, isAssign := ifStmt.Init.(*ast.AssignStmt)
	if !isAssign || init.Tok != token.DEFINE || len(init.Lhs) != 2 || len(init.Rhs) != 1 {
		return guard, false
	}
	detailName, detailOK := identName(init.Lhs[0])
	errName, errOK := identName(init.Lhs[1])
	if !detailOK || !errOK || detailName == "_" {
		return guard, false
	}
	call, isCall := init.Rhs[0].(*ast.CallExpr)
	if !isCall || !isConnectSelector(call.Fun, connectAlias, "NewErrorDetail") || len(call.Args) != 1 {
		return guard, false
	}
	pkgIdent := call.Fun.(*ast.SelectorExpr).X.(*ast.Ident) //nolint:errcheck,forcetypeassert  // isConnectSelector guarantees the selector and ident shapes.
	if !isNilCheck(ifStmt.Cond, errName) {
		return guard, false
	}
	if len(ifStmt.Body.List) != 1 {
		return guard, false
	}
	exprStmt, isExpr := ifStmt.Body.List[0].(*ast.ExprStmt)
	if !isExpr {
		return guard, false
	}
	addCall, isAddCall := exprStmt.X.(*ast.CallExpr)
	if !isAddCall || len(addCall.Args) != 1 {
		return guard, false
	}
	addSel, isAddSel := addCall.Fun.(*ast.SelectorExpr)
	if !isAddSel || addSel.Sel.Name != "AddDetail" {
		return guard, false
	}
	// Target must be a bare identifier to safely duplicate across the assignment.
	targetIdent, targetOK := addSel.X.(*ast.Ident)
	if !targetOK {
		return guard, false
	}
	if argName, argOK := identName(addCall.Args[0]); !argOK || argName != detailName {
		return guard, false
	}
	return detailGuard{constructorPkg: pkgIdent, target: targetIdent, detailName: detailName}, true
}

// withDetailAssign builds `target = target.WithDetail(message)` anchored at pos.
func withDetailAssign(target *ast.Ident, message ast.Expr, pos token.Pos) *ast.AssignStmt {
	lhs := &ast.Ident{NamePos: pos, Name: target.Name}
	receiver := &ast.Ident{NamePos: pos, Name: target.Name}
	return &ast.AssignStmt{
		Lhs:    []ast.Expr{lhs},
		TokPos: pos,
		Tok:    token.ASSIGN,
		Rhs: []ast.Expr{&ast.CallExpr{
			Fun: &ast.SelectorExpr{
				X:   receiver,
				Sel: &ast.Ident{Name: "WithDetail"},
			},
			Args: []ast.Expr{message},
		}},
	}
}

func isNilCheck(cond ast.Expr, name string) bool {
	binary, ok := cond.(*ast.BinaryExpr)
	if !ok || binary.Op != token.EQL {
		return false
	}
	left, leftOK := identName(binary.X)
	right, rightOK := identName(binary.Y)
	if !leftOK || !rightOK {
		return false
	}
	return (left == name && right == identNil) || (left == identNil && right == name)
}

func identName(expr ast.Expr) (string, bool) {
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return "", false
	}
	return ident.Name, true
}
