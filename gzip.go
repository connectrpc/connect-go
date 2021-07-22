package rerpc

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
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

func maybeGzipWriter(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func()) {
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		// Client doesn't want gzip.
		return w, func() {}
	}
	if enc := w.Header().Get("Content-Encoding"); enc != "" {
		// Someone's already compressing the body.
		return w, func() {}
	}
	w.Header().Set("Content-Encoding", "gzip")
	gz := gzWriterPool.Get().(*gzip.Writer)
	gz.Reset(w)
	return &gzipResponseWriter{ResponseWriter: w, gw: gz}, func() {
		gz.Close()           // if we haven't already, close
		gz.Reset(io.Discard) // don't keep references
		gzWriterPool.Put(gz) // return to pool
	}
}

func maybeGzipReader(r *http.Request) (io.Reader, func(), error) {
	// It'd be nice to pool these too, but it's a bit of a pain: we'd need to
	// reset the readers in the pool to use a thread-safe source of valid gzip
	// data.
	if r.Header.Get("Content-Encoding") != "gzip" {
		return r.Body, func() {}, nil
	}
	gr, err := gzip.NewReader(r.Body)
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() {
		_ = gr.Close()
	}
	return gr, cleanup, nil
}
