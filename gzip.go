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
