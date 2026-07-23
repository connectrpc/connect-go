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

// Package connectgzip provides a gzip [connect.Compressor] for the "gzip"
// Content-Encoding.
package connectgzip

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"sync"

	"connectrpc.com/connect/v2"
)

var _ connect.Compressor = (*Compressor)(nil)

// errClosed is returned when a recycled writer or reader is used after
// Close.
var errClosed = errors.New("connectgzip: use after Close")

// Option configures a [Compressor].
type Option interface {
	apply(*Compressor)
}

// WithLevel sets the gzip compression level. Accepts any value valid
// for [compress/gzip.NewWriterLevel] (-2 through 9). Defaults to
// [compress/gzip.DefaultCompression].
func WithLevel(level int) Option {
	return optionFunc(func(c *Compressor) { c.level = level })
}

// Compressor implements [connect.Compressor] using compress/gzip.
type Compressor struct {
	level   int
	writers sync.Pool
	readers sync.Pool
}

// New returns a [Compressor] configured with the given options.
func New(opts ...Option) *Compressor {
	c := &Compressor{level: gzip.DefaultCompression}
	for _, o := range opts {
		o.apply(c)
	}
	return c
}

// Name returns "gzip".
func (c *Compressor) Name() string { return connect.CompressionNameGzip }

// Compress returns a pooled writer that gzip-encodes everything written
// to it onto dst. Close flushes the gzip stream and recycles the writer.
func (c *Compressor) Compress(dst io.Writer) (io.WriteCloser, error) {
	if pooled, ok := c.writers.Get().(*compressWriter); ok {
		pooled.writer.Reset(dst)
		pooled.closed = false
		return pooled, nil
	}
	gzWriter, err := gzip.NewWriterLevel(dst, c.level)
	if err != nil {
		return nil, fmt.Errorf("connectgzip: create writer: %w", err)
	}
	return &compressWriter{compressor: c, writer: gzWriter}, nil
}

// Decompress returns a pooled reader yielding the gzip-decoded form of
// src. The gzip header is read from src before Decompress returns. Close
// recycles the reader without closing src. It is safe to close a reader
// that was not fully consumed.
func (c *Compressor) Decompress(src io.Reader) (io.ReadCloser, error) {
	if pooled, ok := c.readers.Get().(*decompressReader); ok {
		if err := pooled.reader.Reset(src); err != nil {
			// Pool poisoning risk on Reset failure: drop state, do not return.
			return nil, fmt.Errorf("connectgzip: open reader: %w", err)
		}
		pooled.closed = false
		return pooled, nil
	}
	gzReader, err := gzip.NewReader(src)
	if err != nil {
		return nil, fmt.Errorf("connectgzip: open reader: %w", err)
	}
	return &decompressReader{compressor: c, reader: gzReader}, nil
}

type optionFunc func(*Compressor)

func (f optionFunc) apply(c *Compressor) { f(c) }

// compressWriter is a pooled gzip writer. Close returns it to the owning
// [Compressor]'s pool. The gzip writer is not reset on Close: Compress
// resets it onto the next destination, so resetting here would
// reinitialize the deflate state twice per payload.
type compressWriter struct {
	compressor *Compressor
	writer     *gzip.Writer
	closed     bool
}

func (w *compressWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errClosed
	}
	n, err := w.writer.Write(p)
	if err != nil {
		return n, fmt.Errorf("connectgzip: compress: %w", err)
	}
	return n, nil
}

func (w *compressWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.writer.Close(); err != nil {
		// Pool poisoning risk: drop state, do not return to pool.
		return fmt.Errorf("connectgzip: close writer: %w", err)
	}
	w.compressor.writers.Put(w)
	return nil
}

// decompressReader is a pooled gzip reader. Close returns it to the
// owning [Compressor]'s pool. Reset on the next Decompress fully
// reinitializes the inflate state, so a half-consumed reader is safe to
// recycle.
type decompressReader struct {
	compressor *Compressor
	reader     *gzip.Reader
	closed     bool
}

func (r *decompressReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, errClosed
	}
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("connectgzip: decompress: %w", err)
	}
	return n, err
}

func (r *decompressReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if err := r.reader.Close(); err != nil {
		// Pool poisoning risk: drop state, do not return to pool.
		return fmt.Errorf("connectgzip: close reader: %w", err)
	}
	r.compressor.readers.Put(r)
	return nil
}
