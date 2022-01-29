package gzip

import (
	"bytes"
	stdgzip "compress/gzip"
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
				return &stdgzip.Reader{}
			},
		},
		writers: sync.Pool{
			New: func() any {
				return stdgzip.NewWriter(io.Discard)
			},
		},
	}
}

func (c *Compressor) ShouldCompress(bs []byte) bool {
	return len(bs) > c.min
}

func (c *Compressor) GetReader(r io.Reader) (io.ReadCloser, error) {
	gr := c.readers.Get().(*stdgzip.Reader)
	return gr, gr.Reset(r)
}

func (c *Compressor) PutReader(r io.ReadCloser) {
	gr, ok := r.(*stdgzip.Reader)
	if !ok {
		return
	}
	if err := gr.Close(); err != nil { // close if we haven't already
		return
	}
	gr.Reset(bytes.NewReader(emptyGzipBytes)) // don't keep references
	c.readers.Put(gr)
}

func (c *Compressor) GetWriter(w io.Writer) io.WriteCloser {
	gw := c.writers.Get().(*stdgzip.Writer)
	gw.Reset(w)
	return gw
}

func (c *Compressor) PutWriter(w io.WriteCloser) {
	gw, ok := w.(*stdgzip.Writer)
	if !ok {
		return
	}
	if err := gw.Close(); err != nil { // close if we haven't already
		return
	}
	gw.Reset(io.Discard) // don't keep references
	c.writers.Put(gw)
}
