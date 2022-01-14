package compress

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"io"
	"sync"
)

const (
	NameGzip     = "gzip"
	NameIdentity = "identity"

	oneKiB = 1024
)

//go:embed empty.gz
var emptyGzipBytes []byte

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

// GzipCompressor uses the standard library's compress/gzip to implement
// Compressor.
type GzipCompressor struct {
	min     int
	readers sync.Pool
	writers sync.Pool
}

// NewGzip creates a new GzipCompressor. The compressor uses the standard
// library's gzip package with the default compression level, and it doesn't
// compress messages smaller than 1kb.
func NewGzip() *GzipCompressor {
	return &GzipCompressor{
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

func (g *GzipCompressor) ShouldCompress(bs []byte) bool {
	return len(bs) > g.min
}

func (g *GzipCompressor) GetReader(r io.Reader) (io.ReadCloser, error) {
	gr := g.readers.Get().(*gzip.Reader)
	return gr, gr.Reset(r)
}

func (g *GzipCompressor) PutReader(r io.ReadCloser) {
	gr, ok := r.(*gzip.Reader)
	if !ok {
		return
	}
	if err := gr.Close(); err != nil { // close if we haven't already
		return
	}
	gr.Reset(bytes.NewReader(emptyGzipBytes)) // don't keep references
	g.readers.Put(gr)
}

func (g *GzipCompressor) GetWriter(w io.Writer) io.WriteCloser {
	gw := g.writers.Get().(*gzip.Writer)
	gw.Reset(w)
	return gw
}

func (g *GzipCompressor) PutWriter(w io.WriteCloser) {
	gw, ok := w.(*gzip.Writer)
	if !ok {
		return
	}
	if err := gw.Close(); err != nil { // close if we haven't already
		return
	}
	gw.Reset(io.Discard) // don't keep references
	g.writers.Put(gw)
}
