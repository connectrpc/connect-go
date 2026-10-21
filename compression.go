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

import "io"

const (
	compressionGzip     = "gzip"
	compressionIdentity = "identity"
)

// A Decompressor is a reusable wrapper that decompresses an underlying data
// source. The standard library's [*gzip.Reader] implements Decompressor.
type Decompressor interface {
	io.Reader

	// Close closes the Decompressor, but not the underlying data source. It may
	// return an error if the Decompressor wasn't read to EOF.
	Close() error

	// Reset discards the Decompressor's internal state, if any, and prepares it
	// to read from a new source of compressed data.
	Reset(io.Reader) error
}

// A Compressor is a reusable wrapper that compresses data written to an
// underlying sink. The standard library's [*gzip.Writer] implements Compressor.
type Compressor interface {
	io.Writer

	// Close flushes any buffered data to the underlying sink, then closes the
	// Compressor. It must not close the underlying sink.
	Close() error

	// Reset discards the Compressor's internal state, if any, and prepares it to
	// write compressed data to a new sink.
	Reset(io.Writer)
}
