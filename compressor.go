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
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	compressGzip     = "gzip"
	compressIdentity = "identity"
)

const oneKiB = 1024

// To reset gzip readers when returning them to a sync.Pool, we need a source
// of valid gzipped data. Gzip files begin with a 10-byte header, which is
// simple enough to write in a literal - no need to go:embed a file.
var emptyGzipBytes = []byte{
	// Magic number, identifies file type.
	0x1f, 0x8b,
	// Compression method. 0-7 reserved, 8 deflate.
	8,
	// File flags.
	0,
	// 32-bit timestamp.
	0, 0, 0, 0,
	// Compression flags.
	0,
	// Operating system ID, 3 is Unix.
	3,
}

// A Compressor provides compressing readers and writers. The interface is
// designed to let implementations use a sync.Pool.
//
// Additionally, Compressors contain logic to decide whether it's worth
// compressing a given payload. Often, it's not worth burning CPU cycles
// compressing small payloads.
type Compressor interface {
	// NewReadCloser provides a ReadCloser that wraps the given Reader with compression.
	//
	// This ReadCloser must be closed when no longer used.
	NewReadCloser(io.Reader) (io.ReadCloser, error)
	// GetWriteCloser provides a WriteCloser that wraps the given Writer with compression.
	//
	// This WriteCloser must be closed when no longer used.
	// Any data written into the given Writer is not valid until the WriteCloser is closed.
	NewWriteCloser(io.Writer) (io.WriteCloser, error)
	// ShouldCompress says whether or not the given payload should be compressed.
	ShouldCompress([]byte) bool
}

type gzipCompressor struct {
	min     int
	readers sync.Pool
	writers sync.Pool
}

var _ Compressor = (*gzipCompressor)(nil)

func newGzipCompressor() *gzipCompressor {
	return &gzipCompressor{
		min: oneKiB,
		readers: sync.Pool{
			New: func() any {
				// We don't want to use gzip.NewReader, because it requires a source of
				// valid gzipped bytes.
				return &gzip.Reader{}
			},
		},
		writers: sync.Pool{
			New: func() any {
				return gzip.NewWriter(io.Discard)
			},
		},
	}
}

func (c *gzipCompressor) NewReadCloser(reader io.Reader) (io.ReadCloser, error) {
	gzipReader, ok := c.readers.Get().(*gzip.Reader)
	if !ok {
		// this should never happen since we control what goes into the pool
		return nil, fmt.Errorf("expected *gzip.Reader from pool but got %T", gzipReader)
	}
	if err := gzipReader.Reset(reader); err != nil {
		return nil, err
	}
	return pooledGzipReader{
		Reader:         gzipReader,
		gzipCompressor: c,
	}, nil
}

func (c *gzipCompressor) NewWriteCloser(writer io.Writer) (io.WriteCloser, error) {
	gzipWriter, ok := c.writers.Get().(*gzip.Writer)
	if !ok {
		// this should never happen since we control what goes into the pool
		return nil, fmt.Errorf("expected *gzip.Writer from pool but got %T", gzipWriter)
	}
	gzipWriter.Reset(writer)
	return pooledGzipWriter{
		Writer:         gzipWriter,
		gzipCompressor: c,
	}, nil
}

func (c *gzipCompressor) ShouldCompress(bs []byte) bool {
	return len(bs) > c.min
}

// readOnlyCompressors is a read-only interface to a map of named compressors.
type readOnlyCompressors interface {
	Get(string) Compressor
	Contains(string) bool
	// Wordy, but clarifies how this is different from readOnlyCodecs.Names().
	CommaSeparatedNames() string
}

func newReadOnlyCompressors(nameToCompressor map[string]Compressor) readOnlyCompressors {
	names := make([]string, 0, len(nameToCompressor))
	for name := range nameToCompressor {
		names = append(names, name)
	}
	return &compressorMap{
		nameToCompressor:    nameToCompressor,
		commaSeparatedNames: strings.Join(names, ","),
	}
}

// compressorMap implements readOnlyCompressors
type compressorMap struct {
	nameToCompressor    map[string]Compressor
	commaSeparatedNames string
}

func (m *compressorMap) Get(name string) Compressor {
	if name == "" || name == compressIdentity {
		return nil
	}
	return m.nameToCompressor[name]
}

func (m *compressorMap) Contains(name string) bool {
	_, ok := m.nameToCompressor[name]
	return ok
}

func (m *compressorMap) CommaSeparatedNames() string {
	return m.commaSeparatedNames
}

// pooledGzipReader wraps a gzipReader and a gzipCompressor, which contains sync.Pools.
//
// This allows us to override Close() to put the gzipReader back into the corresponding Pool.
type pooledGzipReader struct {
	*gzip.Reader

	gzipCompressor *gzipCompressor
}

func (r pooledGzipReader) Close() error {
	if err := r.Reader.Close(); err != nil {
		return err
	}
	// Don't keep references
	if err := r.Reader.Reset(bytes.NewReader(emptyGzipBytes)); err != nil {
		return err
	}
	r.gzipCompressor.readers.Put(r.Reader)
	return nil
}

// pooledGzipWriter wraps a gzipWriter and a gzipCompressor, which contains sync.Pools.
//
// This allows us to override Close() to put the gzipWriter back into the corresponding Pool.
type pooledGzipWriter struct {
	*gzip.Writer

	gzipCompressor *gzipCompressor
}

func (w pooledGzipWriter) Close() error {
	if err := w.Writer.Close(); err != nil {
		return err
	}
	// Don't keep references
	w.Writer.Reset(io.Discard)
	w.gzipCompressor.writers.Put(w.Writer)
	return nil
}
