package rerpc

import (
	"compress/gzip"
	"io"
	"net/http"
	"sync"
)

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
