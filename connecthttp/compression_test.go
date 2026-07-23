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
	"io"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
)

func TestCompressionOption(t *testing.T) {
	t.Parallel()

	apply := func(opts ...Option) *options {
		o := defaultOptions()
		for _, opt := range opts {
			opt.apply(&o)
		}
		return &o
	}
	checkPools := func(t *testing.T, opts *options) {
		t.Helper()
		assert.Equal(t, len(opts.compressorNames), len(opts.compressors))
		for _, name := range opts.compressorNames {
			assert.NotNil(t, opts.compressors[name])
		}
	}

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		opts := apply()
		assert.Equal(t, opts.compressorNames, []string{connect.CompressionNameGzip})
		checkPools(t, opts)
	})
	t.Run("withCompressors registers and accepts", func(t *testing.T) {
		t.Parallel()
		opts := apply(WithCompressor(identityCompressor{}))
		assert.Equal(t, opts.compressorNames, []string{connect.CompressionNameGzip, connect.CompressionNameIdentity})
		checkPools(t, opts)
	})
	t.Run("withNoCompression-disables", func(t *testing.T) {
		t.Parallel()
		opts := apply(WithNoCompression())
		assert.Equal(t, opts.compressorNames, nil)
		assert.Equal(t, len(opts.compressors), 0)
	})
	t.Run("withAcceptCompression-selects-name", func(t *testing.T) {
		t.Parallel()
		opts := apply(WithAcceptCompression("br"))
		assert.Equal(t, opts.compressorNames, []string{connect.CompressionNameGzip, "br"})
	})
	t.Run("withAcceptCompression-empty-name-noop", func(t *testing.T) {
		t.Parallel()
		opts := apply(WithAcceptCompression(""))
		assert.Equal(t, opts.compressorNames, []string{connect.CompressionNameGzip})
	})
	t.Run("withAcceptCompression-deduplicates", func(t *testing.T) {
		t.Parallel()
		opts := apply(WithAcceptCompression(connect.CompressionNameGzip))
		assert.Equal(t, opts.compressorNames, []string{connect.CompressionNameGzip})
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
