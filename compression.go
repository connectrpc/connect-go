// Copyright 2021-2022 Buf Technologies, Inc.
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
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	compressionGzip     = "gzip"
	compressionIdentity = "identity"
)

// A Decompressor is a reusable wrapper that decompresses an underlying data
// source. The standard library's *gzip.Reader implements Decompressor.
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
// underlying sink. The standard library's *gzip.Writer implements Compressor.
type Compressor interface {
	io.Writer

	// Close flushes any buffered data to the underlying sink, then closes the
	// Compressor. It must not close the underlying sink.
	Close() error

	// Reset discards the Compressor's internal state, if any, and prepares it to
	// write compressed data to a new sink.
	Reset(io.Writer)
}

type compressionPool interface {
	GetReader(io.Reader) (io.Reader, error)
	PutReader(io.Reader) error

	GetWriter(io.Writer) (io.Writer, error)
	PutWriter(io.Writer) error
}

func newCompressionPool[D Decompressor, C Compressor](
	newDecompressor func() D,
	newCompressor func() C,
) compressionPool {
	return &typedCompressionPool[D, C]{
		decompressors: sync.Pool{
			New: func() any { return newDecompressor() },
		},
		compressors: sync.Pool{
			New: func() any { return newCompressor() },
		},
	}
}

type typedCompressionPool[D Decompressor, C Compressor] struct {
	decompressors sync.Pool
	compressors   sync.Pool
}

func (c *typedCompressionPool[D, C]) GetReader(reader io.Reader) (io.Reader, error) {
	decompressor, ok := c.decompressors.Get().(D)
	if !ok {
		var expected D
		return nil, fmt.Errorf("expected %T, got incorrect type from pool", expected)
	}
	return decompressor, decompressor.Reset(reader)
}

func (c *typedCompressionPool[D, C]) PutReader(reader io.Reader) error {
	decompressor, ok := reader.(D)
	if !ok {
		var expected D
		return fmt.Errorf("expected %T, got %T", expected, reader)
	}
	if err := decompressor.Close(); err != nil {
		return err
	}
	// While it's in the pool, we don't want the decompressor to retain a
	// reference to the underlying reader. However, most decompressors attempt to
	// read some header data from the new data source when Reset; since we don't
	// know the compression format, we can't provide a valid header. Since we
	// also reset the decompressor when it's pulled out of the pool, we can
	// ignore errors here.
	_ = decompressor.Reset(strings.NewReader(""))
	c.decompressors.Put(decompressor)
	return nil
}

func (c *typedCompressionPool[D, C]) GetWriter(writer io.Writer) (io.Writer, error) {
	compressor, ok := c.compressors.Get().(C)
	if !ok {
		var expected C
		return nil, fmt.Errorf("expected %T, got incorrect type from pool", expected)
	}
	compressor.Reset(writer)
	return compressor, nil
}

func (c *typedCompressionPool[D, C]) PutWriter(writer io.Writer) error {
	compressor, ok := writer.(C)
	if !ok {
		var expected C
		return fmt.Errorf("expected %T, got %T", expected, writer)
	}
	if err := compressor.Close(); err != nil {
		return err
	}
	compressor.Reset(io.Discard) // don't keep references
	c.compressors.Put(compressor)
	return nil
}

// readOnlyCompressionPools is a read-only interface to a map of named
// compressionPools.
type readOnlyCompressionPools interface {
	Get(string) compressionPool
	Contains(string) bool
	// Wordy, but clarifies how this is different from readOnlyCodecs.Names().
	CommaSeparatedNames() string
}

func newReadOnlyCompressionPools(pools map[string]compressionPool) readOnlyCompressionPools {
	known := make([]string, 0, len(pools))
	for name := range pools {
		known = append(known, name)
	}
	return &namedCompressionPools{
		nameToPools:         pools,
		commaSeparatedNames: strings.Join(known, ","),
	}
}

type namedCompressionPools struct {
	nameToPools         map[string]compressionPool
	commaSeparatedNames string
}

func (m *namedCompressionPools) Get(name string) compressionPool {
	if name == "" || name == compressionIdentity {
		return nil
	}
	return m.nameToPools[name]
}

func (m *namedCompressionPools) Contains(name string) bool {
	_, ok := m.nameToPools[name]
	return ok
}

func (m *namedCompressionPools) CommaSeparatedNames() string {
	return m.commaSeparatedNames
}
