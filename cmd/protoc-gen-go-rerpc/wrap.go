package main

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/compiler/protogen"
)

// Raggedy comments in the generated code are driving me insane. This
// word-wrapping function is ruinously inefficient, but it gets the job done.
func comment(g *protogen.GeneratedFile, elems ...interface{}) {
	const width = 77 // leave room for "// "
	text := &bytes.Buffer{}
	for _, el := range elems {
		switch el := el.(type) {
		case protogen.GoIdent:
			fmt.Fprint(text, g.QualifiedGoIdent(el))
		default:
			fmt.Fprint(text, el)
		}
	}
	words := strings.Fields(text.String())
	text.Reset()
	var pos int
	for _, word := range words {
		n := utf8.RuneCountInString(word)
		if pos > 0 && pos+n+1 > width {
			g.P("// ", text.String())
			text.Reset()
			pos = 0
		}
		if pos > 0 {
			text.WriteRune(' ')
			pos += 1
		}
		text.WriteString(word)
		pos += n
	}
	if text.Len() > 0 {
		g.P("// ", text.String())
	}
}
