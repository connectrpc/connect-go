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

package internal

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// Printer is a simple interface for formatting human-readable messages.
type Printer interface {
	// Printf formats the given message and arguments. A newline
	// is automatically added, so it is not necessary to include
	// an explicit "\n" at the end.
	Printf(msg string, args ...any)
	// PrefixPrintf is just like Printf except it will print
	// the given prefix followed by ": " before printing the
	// messages and arguments.
	PrefixPrintf(prefix, msg string, args ...any)
}

// NewPrinter returns a thread-safe printer that prints messages
// to the given writer. The returned printer may safely be used
// from concurrent goroutines, even if the given writer is not
// safe for concurrent use.
func NewPrinter(w io.Writer) Printer {
	return &safePrinter{w: &peekWriter{w: w}}
}

// SimplePrinter is a non-thread-safe printer that stores the
// printed messages in a slice.
type SimplePrinter struct {
	Messages []string
}

func (l *SimplePrinter) Printf(msg string, args ...any) {
	line := fmt.Sprintf(msg, args...)
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	l.Messages = append(l.Messages, line)
}

func (l *SimplePrinter) PrefixPrintf(prefix, msg string, args ...any) {
	msg = fmt.Sprintf(msg, args...)
	line := fmt.Sprintf("%s: %s", prefix, msg)
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	l.Messages = append(l.Messages, line)
}

// safePrinter is a thread-safe printer.
type safePrinter struct {
	mu sync.Mutex
	w  *peekWriter
}

func (p *safePrinter) Printf(msg string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintf(p.w, msg, args...)
	if p.w.last != '\n' {
		_, _ = p.w.Write([]byte{'\n'})
	}
}

func (p *safePrinter) PrefixPrintf(prefix, msg string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintf(p.w, "%s: ", prefix)
	_, _ = fmt.Fprintf(p.w, msg, args...)
	if p.w.last != '\n' {
		_, _ = p.w.Write([]byte{'\n'})
	}
}

// peekWriter is a writer that can peek at the last byte written.
type peekWriter struct {
	w    io.Writer
	last byte
}

func (p *peekWriter) Write(data []byte) (int, error) {
	n, err := p.w.Write(data)
	if n > 0 {
		p.last = data[n-1]
	}
	return n, err
}
