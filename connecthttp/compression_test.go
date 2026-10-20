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

package connecthttp

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
)

func TestCompressionOption(t *testing.T) {
	t.Parallel()

	// preference returns the negotiated preference order, most preferred first.
	preference := func(options ...Option) string {
		opts := defaultOptions()
		for _, option := range options {
			option.apply(&opts)
		}
		return newReadOnlyCompressionPools(opts.compressors).CommaSeparatedNames()
	}
	const (
		gzip     = connect.CompressionNameGzip
		identity = connect.CompressionNameIdentity
	)

	t.Run("defaults to gzip", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, preference(), gzip)
	})
	t.Run("first listed is most preferred", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, preference(WithCompressors(identityCompressor{}, stubCompressor(gzip))), identity+","+gzip)
	})
	t.Run("replaces the defaults", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, preference(WithCompressors(identityCompressor{})), identity)
	})
	t.Run("no arguments disables compression", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, preference(WithCompressors()), "")
	})
	t.Run("last call wins", func(t *testing.T) {
		t.Parallel()
		opts := []Option{WithCompressors(identityCompressor{}), WithCompressors(stubCompressor(gzip))}
		assert.Equal(t, preference(opts...), gzip)
	})
	t.Run("repeated name keeps its first entry", func(t *testing.T) {
		t.Parallel()
		opts := []Option{WithCompressors(identityCompressor{}, stubCompressor(gzip), identityCompressor{})}
		assert.Equal(t, preference(opts...), identity+","+gzip)
	})
}

// identityCompressor is a test [connect.Compressor] that performs no
// compression: Compress and Decompress pass bytes through unchanged.
type identityCompressor struct{}

func (identityCompressor) Name() string { return connect.CompressionNameIdentity }

func (identityCompressor) Compress(dst io.Writer) (io.WriteCloser, error) {
	return identityWriteCloser{dst}, nil
}

func (identityCompressor) Decompress(src io.Reader) (io.ReadCloser, error) {
	return io.NopCloser(src), nil
}

// identityWriteCloser adapts an io.Writer to io.WriteCloser with a no-op Close.
type identityWriteCloser struct{ io.Writer }

func (identityWriteCloser) Close() error { return nil }

// TestDecompressBoundsOversizedRead verifies that rejecting a message that
// decompresses past readMaxBytes reads at most discardLimit more.
func TestDecompressBoundsOversizedRead(t *testing.T) {
	t.Parallel()
	const readMaxBytes = 1024
	bomb := &countingReader{reader: io.LimitReader(zeroReader{}, 64<<20)}
	pool := newCompressionPool(stubDecompressor{bomb})
	err := pool.Decompress(&bytes.Buffer{}, &bytes.Buffer{}, readMaxBytes)
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
	assert.True(t, bomb.n <= readMaxBytes+1+discardLimit, assert.Sprintf("read %d decompressed bytes", bomb.n))
}

// stubDecompressor is a test [connect.Compressor] whose Decompress returns
// reader regardless of the source.
type stubDecompressor struct{ reader io.Reader }

func (stubDecompressor) Name() string { return connect.CompressionNameGzip }

func (stubDecompressor) Compress(io.Writer) (io.WriteCloser, error) {
	return nil, errors.New("not implemented")
}

func (d stubDecompressor) Decompress(io.Reader) (io.ReadCloser, error) {
	return io.NopCloser(d.reader), nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type countingReader struct {
	reader io.Reader
	n      int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.n += int64(n)
	return n, err
}
