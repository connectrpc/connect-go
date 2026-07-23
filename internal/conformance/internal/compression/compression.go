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

package compression

import (
	"fmt"
	"io"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectgzip"
	conformancev1 "connectrpc.com/connect/v2/internal/conformance/internal/gen/connectrpc/conformance/v1"
)

// The IANA names for supported compression algorithms.
const (
	Identity = "identity"
	Gzip     = "gzip"
	Brotli   = "br"
	Deflate  = "deflate"
	Snappy   = "snappy"
	Zstd     = "zstd"
)

// GetCompressor returns a [connect.Compressor] for the given compression
// algorithm. The v2 reference supports identity and gzip; the remaining
// algorithms are not yet ported.
func GetCompressor(compression conformancev1.Compression) (connect.Compressor, error) {
	switch compression {
	case conformancev1.Compression_COMPRESSION_UNSPECIFIED, conformancev1.Compression_COMPRESSION_IDENTITY:
		return identityCompressor{}, nil
	case conformancev1.Compression_COMPRESSION_GZIP:
		return connectgzip.New(), nil
	default:
		return nil, fmt.Errorf("unsupported compression scheme %v", compression)
	}
}

// GetDecompressor returns a [connect.Compressor] for the given compression
// algorithm. In v2 a single type both compresses and decompresses, so this
// mirrors [GetCompressor].
func GetDecompressor(compression conformancev1.Compression) (connect.Compressor, error) {
	return GetCompressor(compression)
}

// identityCompressor is a no-op [connect.Compressor] that passes bytes through
// unchanged.
type identityCompressor struct{}

func (identityCompressor) Name() string { return Identity }

func (identityCompressor) Compress(dst io.Writer) (io.WriteCloser, error) {
	return nopWriteCloser{dst}, nil
}

func (identityCompressor) Decompress(src io.Reader) (io.ReadCloser, error) {
	return io.NopCloser(src), nil
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }
