package gzip

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"io"
	"sync"
)

const (
	Name = "gzip"

	oneKiB = 1024
)

//go:embed empty.gz
var emptyGzipBytes []byte

// Compressor uses the standard library's compress/gzip to implement
// compress.Compressor.
type Compressor struct {
	min     int
	readers sync.Pool
	writers sync.Pool
}

// New creates a new Compressor. The compressor uses the standard library's
// gzip package with the default compression level, and it doesn't compress
// messages smaller than 1kb.
func New() *Compressor {
	return &Compressor{
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

func (c *Compressor) ShouldCompress(bs []byte) bool {
	return len(bs) > c.min
}

func (c *Compressor) GetReader(r io.Reader) (io.ReadCloser, error) {
	gzipReader, ok := c.readers.Get().(*gzip.Reader)
	if !ok {
		return gzip.NewReader(r)
	}
	return gzipReader, gzipReader.Reset(r)
}

func (c *Compressor) PutReader(r io.ReadCloser) {
	gzipReader, ok := r.(*gzip.Reader)
	if !ok {
		return
	}
	if err := gzipReader.Close(); err != nil { // close if we haven't already
		return
	}
	gzipReader.Reset(bytes.NewReader(emptyGzipBytes)) // don't keep references
	c.readers.Put(gzipReader)
}

func (c *Compressor) GetWriter(w io.Writer) io.WriteCloser {
	gzipWriter, ok := c.writers.Get().(*gzip.Writer)
	if !ok {
		return gzip.NewWriter(w)
	}
	gzipWriter.Reset(w)
	return gzipWriter
}

func (c *Compressor) PutWriter(w io.WriteCloser) {
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
