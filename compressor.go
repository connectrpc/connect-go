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
	GetReader(io.Reader) (io.ReadCloser, error)
	PutReader(io.ReadCloser)

	ShouldCompress([]byte) bool
	GetWriter(io.Writer) io.WriteCloser
	PutWriter(io.WriteCloser)
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

func (c *gzipCompressor) ShouldCompress(bs []byte) bool {
	return len(bs) > c.min
}

func (c *gzipCompressor) GetReader(r io.Reader) (io.ReadCloser, error) {
	gzipReader, ok := c.readers.Get().(*gzip.Reader)
	if !ok {
		return gzip.NewReader(r)
	}
	return gzipReader, gzipReader.Reset(r)
}

func (c *gzipCompressor) PutReader(r io.ReadCloser) {
	gzipReader, ok := r.(*gzip.Reader)
	if !ok {
		return
	}
	if err := gzipReader.Close(); err != nil { // close if we haven't already
		return
	}
	if err := gzipReader.Reset(bytes.NewReader(emptyGzipBytes)); err != nil { // don't keep references
		return
	}
	c.readers.Put(gzipReader)
}

func (c *gzipCompressor) GetWriter(w io.Writer) io.WriteCloser {
	gzipWriter, ok := c.writers.Get().(*gzip.Writer)
	if !ok {
		return gzip.NewWriter(w)
	}
	gzipWriter.Reset(w)
	return gzipWriter
}

func (c *gzipCompressor) PutWriter(w io.WriteCloser) {
	gzipWriter, ok := w.(*gzip.Writer)
	if !ok {
		return
	}
	if err := gzipWriter.Close(); err != nil { // close if we haven't already
		return
	}
	gzipWriter.Reset(io.Discard) // don't keep references
	c.writers.Put(gzipWriter)
}

// readOnlyCompressors is a read-only interface to a map of named compressors.
type readOnlyCompressors interface {
	Get(string) Compressor
	Contains(string) bool
	// Wordy, but clarifies how this is different from readOnlyCodecs.Names().
	CommaSeparatedNames() string
}

type compressorMap struct {
	compressors map[string]Compressor
	names       string
}

func newReadOnlyCompressors(compressors map[string]Compressor) *compressorMap {
	known := make([]string, 0, len(compressors))
	for name := range compressors {
		known = append(known, name)
	}
	return &compressorMap{
		compressors: compressors,
		names:       strings.Join(known, ","),
	}
}

func (m *compressorMap) Get(name string) Compressor {
	if name == "" || name == compressIdentity {
		return nil
	}
	return m.compressors[name]
}

func (m *compressorMap) Contains(name string) bool {
	_, ok := m.compressors[name]
	return ok
}

func (m *compressorMap) CommaSeparatedNames() string {
	return m.names
}
