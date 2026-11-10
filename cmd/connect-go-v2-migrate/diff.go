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
	"fmt"
	"go/scanner"
	"go/token"
	"strings"
)

// diffContext is the number of unchanged context lines shown around each change.
const diffContext = 3

// ANSI colors for diff output, used only when color is requested.
const (
	colorReset = "\x1b[0m"
	colorRed   = "\x1b[31m"
	colorGreen = "\x1b[32m"
	colorCyan  = "\x1b[36m"
)

// diffLine is one line of the edit script: a tag (' ' unchanged, '-' removed,
// '+' added), the text, and the 1-based line numbers it occupies in the old (a)
// and new (b) files (0 when the line is absent from that side).
type diffLine struct {
	tag  byte
	text string
	aNum int
	bNum int
}

// unifiedDiff renders the change between a and b as a unified diff (context
// lines, @@ hunk headers, -/+ lines colorized when color is true) for CLI output.
func unifiedDiff(name string, a, b []byte, color bool) string {
	lines := editScript(strings.Split(string(a), "\n"), strings.Split(string(b), "\n"))

	var builder strings.Builder
	builder.WriteString("--- ")
	builder.WriteString(name)
	builder.WriteString("\n+++ ")
	builder.WriteString(name)
	builder.WriteString("\n")
	for _, hunk := range hunks(lines) {
		writeHunk(&builder, lines[hunk[0]:hunk[1]], color)
	}
	return builder.String()
}

// editScript produces the full tagged line sequence via a longest-common-
// subsequence diff.
func editScript(aLines, bLines []string) []diffLine {
	aLen, bLen := len(aLines), len(bLines)
	// lcs[i][j] = LCS length of aLines[i:] and bLines[j:], filled bottom-up so
	// the walk below runs forward.
	lcs := make([][]int, aLen+1)
	for i := range lcs {
		lcs[i] = make([]int, bLen+1)
	}
	for i := aLen - 1; i >= 0; i-- {
		for bIdx := bLen - 1; bIdx >= 0; bIdx-- {
			if aLines[i] == bLines[bIdx] {
				lcs[i][bIdx] = lcs[i+1][bIdx+1] + 1
				continue
			}
			lcs[i][bIdx] = max(lcs[i+1][bIdx], lcs[i][bIdx+1])
		}
	}

	var lines []diffLine
	aIdx, bIdx := 0, 0
	for aIdx < aLen && bIdx < bLen {
		switch {
		case aLines[aIdx] == bLines[bIdx]:
			lines = append(lines, diffLine{tag: ' ', text: aLines[aIdx], aNum: aIdx + 1, bNum: bIdx + 1})
			aIdx++
			bIdx++
		case lcs[aIdx+1][bIdx] >= lcs[aIdx][bIdx+1]:
			lines = append(lines, diffLine{tag: '-', text: aLines[aIdx], aNum: aIdx + 1})
			aIdx++
		default:
			lines = append(lines, diffLine{tag: '+', text: bLines[bIdx], bNum: bIdx + 1})
			bIdx++
		}
	}
	for ; aIdx < aLen; aIdx++ {
		lines = append(lines, diffLine{tag: '-', text: aLines[aIdx], aNum: aIdx + 1})
	}
	for ; bIdx < bLen; bIdx++ {
		lines = append(lines, diffLine{tag: '+', text: bLines[bIdx], bNum: bIdx + 1})
	}
	return lines
}

// hunks groups the edit script into [start,end) ranges, each a run of changes
// plus diffContext lines on either side; touching windows merge into one hunk.
func hunks(lines []diffLine) [][2]int {
	count := len(lines)
	visible := make([]bool, count)
	for i := range lines {
		if lines[i].tag == ' ' {
			continue
		}
		low := max(i-diffContext, 0)
		high := min(i+diffContext, count-1)
		for k := low; k <= high; k++ {
			visible[k] = true
		}
	}

	var ranges [][2]int
	for i := 0; i < count; {
		if !visible[i] {
			i++
			continue
		}
		start := i
		for i < count && visible[i] {
			i++
		}
		ranges = append(ranges, [2]int{start, i})
	}
	return ranges
}

func writeHunk(builder *strings.Builder, lines []diffLine, color bool) {
	aStart, aCount, bStart, bCount := bounds(lines)
	header := fmt.Sprintf("@@ -%d,%d +%d,%d @@", aStart, aCount, bStart, bCount)
	if color {
		header = colorCyan + header + colorReset
	}
	builder.WriteString(header)
	builder.WriteString("\n")
	for _, line := range lines {
		builder.WriteString(colorize(line, color))
		builder.WriteString("\n")
	}
}

// bounds returns the start line and count for each side of a hunk's @@ header.
func bounds(lines []diffLine) (aStart, aCount, bStart, bCount int) {
	for _, line := range lines {
		if line.aNum != 0 {
			if aCount == 0 {
				aStart = line.aNum
			}
			aCount++
		}
		if line.bNum != 0 {
			if bCount == 0 {
				bStart = line.bNum
			}
			bCount++
		}
	}
	return aStart, aCount, bStart, bCount
}

func colorize(line diffLine, color bool) string {
	text := string(line.tag) + line.text
	if !color {
		return text
	}
	switch line.tag {
	case '-':
		return colorRed + text + colorReset
	case '+':
		return colorGreen + text + colorReset
	default:
		return text
	}
}

// positionMapper moves positions in an original file to the rewritten one.
type positionMapper struct {
	aLines, bLines []string
	script         []diffLine
	scriptIndex    map[int]int // aNum -> index in script
}

func newPositionMapper(a, b []byte) *positionMapper {
	mapper := &positionMapper{
		aLines:      strings.Split(string(a), "\n"),
		bLines:      strings.Split(string(b), "\n"),
		scriptIndex: map[int]int{},
	}
	mapper.script = editScript(mapper.aLines, mapper.bLines)
	for index, line := range mapper.script {
		if line.aNum > 0 {
			mapper.scriptIndex[line.aNum] = index
		}
	}
	return mapper
}

func (m *positionMapper) remap(line, col int) (int, int) {
	index, ok := m.scriptIndex[line]
	if !ok {
		return line, col
	}
	if m.script[index].tag == ' ' {
		return m.script[index].bNum, col
	}
	start, end := index, index
	for start > 0 && m.script[start-1].tag != ' ' {
		start--
	}
	for end < len(m.script) && m.script[end].tag != ' ' {
		end++
	}
	var removed, added []diffLine
	for _, entry := range m.script[start:end] {
		if entry.tag == '-' {
			removed = append(removed, entry)
		} else {
			added = append(added, entry)
		}
	}
	if len(added) == 0 {
		if end < len(m.script) {
			return m.script[end].bNum, 1
		}
		return max(1, len(m.bLines)), 1
	}
	candidates := added
	if len(removed) == len(added) {
		// A one-for-one swap tries the counterpart line first.
		for position, entry := range removed {
			if entry.aNum == line {
				candidates = append([]diffLine{added[position]}, added...)
				break
			}
		}
	}
	if token := identifierAt(m.aLines[line-1], col); token != "" {
		for _, entry := range candidates {
			if found := indexIdentifier(entry.text, token); found >= 0 {
				return entry.bNum, found + 1
			}
		}
	}
	first := candidates[0]
	return first.bNum, len(first.text) - len(strings.TrimLeft(first.text, " \t")) + 1
}

// identifierAt returns the identifier starting at the 1-based col. A qualified
// name yields only its package, which survives rewrites of the selector.
func identifierAt(line string, col int) string {
	for _, ident := range scanIdentifiers(line) {
		if ident.offset == col-1 {
			return ident.name
		}
	}
	return ""
}

// indexIdentifier finds token as a whole identifier, so `mux` doesn't match
// inside `demux` or a string.
func indexIdentifier(text, token string) int {
	for _, ident := range scanIdentifiers(text) {
		if ident.name == token {
			return ident.offset
		}
	}
	return -1
}

type identifier struct {
	name   string
	offset int
}

func scanIdentifiers(line string) []identifier {
	src := []byte(line)
	file := token.NewFileSet().AddFile("", -1, len(src))
	var lineScanner scanner.Scanner
	// A lone line may not be valid Go, so scan errors are ignored.
	lineScanner.Init(file, src, nil, 0)
	var identifiers []identifier
	for {
		pos, tok, lit := lineScanner.Scan()
		if tok == token.EOF {
			return identifiers
		}
		if tok == token.IDENT {
			identifiers = append(identifiers, identifier{name: lit, offset: file.Offset(pos)})
		}
	}
}
