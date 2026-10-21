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

package connect

import (
	"errors"
	"io"
	"net/http"
	"sync"

	connectv2 "connectrpc.com/connect/v2"
)

var errCompressorClosed = errors.New("use after Close")

// compressorToConnectv2 adapts a pair of v1 compressor constructors to a
// [connectv2.Compressor]. Like v1, it pools compressors and decompressors and
// recycles them on Close.
func compressorToConnectv2(
	name string,
	newDecompressor func() Decompressor,
	newCompressor func() Compressor,
) connectv2.Compressor {
	return &v2CompressorAdapter{
		name:            name,
		newDecompressor: newDecompressor,
		newCompressor:   newCompressor,
	}
}

type v2CompressorAdapter struct {
	name            string
	newDecompressor func() Decompressor
	newCompressor   func() Compressor
	compressors     sync.Pool
	decompressors   sync.Pool
}

func (a *v2CompressorAdapter) Name() string { return a.name }

func (a *v2CompressorAdapter) Compress(dst io.Writer) (io.WriteCloser, error) {
	if a.newCompressor == nil {
		return nil, errorf(CodeInternal, "compressor %q does not support compression", a.name)
	}
	compressor, ok := a.compressors.Get().(Compressor)
	if !ok {
		compressor = a.newCompressor()
	}
	compressor.Reset(dst)
	return &compressWriter{adapter: a, compressor: compressor}, nil
}

func (a *v2CompressorAdapter) Decompress(src io.Reader) (io.ReadCloser, error) {
	if a.newDecompressor == nil {
		return nil, errorf(CodeInternal, "compressor %q does not support decompression", a.name)
	}
	decompressor, ok := a.decompressors.Get().(Decompressor)
	if !ok {
		decompressor = a.newDecompressor()
	}
	if err := decompressor.Reset(src); err != nil {
		_ = decompressor.Close()
		return nil, err
	}
	return &decompressReader{adapter: a, decompressor: decompressor}, nil
}

type compressWriter struct {
	adapter    *v2CompressorAdapter
	compressor Compressor
	closed     bool
}

func (w *compressWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errCompressorClosed
	}
	return w.compressor.Write(p)
}

func (w *compressWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.compressor.Close(); err != nil {
		return err
	}
	w.compressor.Reset(io.Discard) // don't keep references
	w.adapter.compressors.Put(w.compressor)
	return nil
}

type decompressReader struct {
	adapter      *v2CompressorAdapter
	decompressor Decompressor
	closed       bool
}

func (r *decompressReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, errCompressorClosed
	}
	return r.decompressor.Read(p)
}

func (r *decompressReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if err := r.decompressor.Close(); err != nil {
		return err
	}
	// While it's in the pool, we don't want the decompressor to retain a
	// reference to the underlying reader. Most decompressors read header data
	// on Reset, so the error is expected and ignored.
	_ = r.decompressor.Reset(http.NoBody)
	r.adapter.decompressors.Put(r.decompressor)
	return nil
}
