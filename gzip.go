package rerpc

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"io"
	"net/http"
	"sync"
)

//go:embed empty.gz
var emptyGzipBytes []byte

var gzWriterPool = sync.Pool{
	New: func() interface{} {
		return gzip.NewWriter(io.Discard)
	},
}

func getGzipWriter(w io.Writer) *gzip.Writer {
	gw := gzWriterPool.Get().(*gzip.Writer)
	gw.Reset(w)
	return gw
}

func putGzipWriter(gw *gzip.Writer) {
	gw.Close()           // close if we haven't already
	gw.Reset(io.Discard) // don't keep references
	gzWriterPool.Put(gw)
}

var gzReaderPool = sync.Pool{
	New: func() interface{} {
		// We don't want to use gzip.NewReader, because it requires a source of
		// valid gzipped bytes.
		var r gzip.Reader
		return &r
	},
}

func getGzipReader(r io.Reader) (*gzip.Reader, error) {
	gr := gzReaderPool.Get().(*gzip.Reader)
	return gr, gr.Reset(r)
}

func putGzipReader(gr *gzip.Reader) {
	gr.Close()                                // close if we haven't already
	gr.Reset(bytes.NewReader(emptyGzipBytes)) // don't keep references
	gzReaderPool.Put(gr)
}

// isWorthCompressing checks whether compression is worthwhile. Very short
// messages and messages unlikely to be compress significantly aren't worth
// burning CPU on.
func isWorthCompressing(raw []byte) bool {
	return len(raw) > 1024
}

// Verify we're implementing these interfaces at compile time.
var (
	_ http.ResponseWriter = &gzipResponseWriter{}
	_ http.Flusher        = &gzipResponseWriter{}
)

type gzipResponseWriter struct {
	http.ResponseWriter

	gw *gzip.Writer
}

func (w *gzipResponseWriter) Flush() {
	w.gw.Flush()
	f, ok := w.ResponseWriter.(http.Flusher)
	if ok {
		f.Flush()
	}
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	w.ResponseWriter.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.gw.Write(b)
}
