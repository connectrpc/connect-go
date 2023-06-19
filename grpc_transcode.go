// Copyright 2021-2023 Buf Technologies, Inc.
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
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"net/textproto"
	"strings"
)

func parseTypeEncoding(header http.Header) (typ, enc string, ok bool) {
	contentType := canonicalizeContentType(getHeaderCanonical(header, headerContentType))

	typ, enc, found := strings.Cut(contentType, "+")
	if !found {
		enc = "proto"
	}
	ok = typ == grpcContentTypeDefault ||
		typ == grpcWebContentTypeDefault ||
		// connect unary
		strings.HasPrefix(typ, connectUnaryContentTypePrefix) ||
		// connect streaming
		typ == connectStreamingContentTypeDefault

	return typ, enc, ok
}

type readCloser struct {
	io.Reader
	io.Closer
}

func translateGRPCWebToGRPC(r *http.Request, typ, enc string) bool {
	//
	r.ProtoMajor = 2
	r.ProtoMinor = 0

	r.Header.Del("Content-Length")
	r.Header.Set("Content-Type", grpcContentTypeDefault+"+"+enc)

	if typ == grpcWebTextContentTypeDefault {
		body := base64.NewDecoder(base64.StdEncoding, r.Body)
		r.Body = readCloser{body, r.Body}
	}

	return true
}

type grpcWebResponseWriter struct {
	http.ResponseWriter
	typ, enc string

	seenHeaders map[string]bool
	wroteHeader bool
}

func newGRPCWebResponseWriter(w http.ResponseWriter, typ, enc string) *grpcWebResponseWriter {
	if typ == grpcWebTextContentTypeDefault {
		w = newBase64ResponseWriter(w)
	}

	return &grpcWebResponseWriter{
		ResponseWriter: w,
		typ:            typ,
		enc:            enc,
	}
}

func (w *grpcWebResponseWriter) seeHeaders() {
	hdr := w.Header()
	hdr.Set("Content-Type", w.typ+"+"+w.enc) // override content-type

	keys := make(map[string]bool, len(hdr))
	for k := range hdr {
		if strings.HasPrefix(k, http.TrailerPrefix) {
			continue
		}
		keys[k] = true
	}
	w.seenHeaders = keys
	w.wroteHeader = true
}

func (w *grpcWebResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.seeHeaders()
	}
	return w.ResponseWriter.Write(b)
}

func (w *grpcWebResponseWriter) WriteHeader(code int) {
	w.seeHeaders()
	w.ResponseWriter.WriteHeader(code)
}

func (w *grpcWebResponseWriter) flushWithTrailers() {
	if w.wroteHeader {
		// Write trailers only if message has been sent.
		if err := w.writeTrailer(); err != nil {
			return // nothing
		}
	}
	flushWriter(w.ResponseWriter)
}

func (w *grpcWebResponseWriter) Flush() {
	flushWriter(w.ResponseWriter)
}

func (w *grpcWebResponseWriter) writeTrailer() error {
	hdr := w.Header()

	tr := make(http.Header, len(hdr)-len(w.seenHeaders)+1)
	for key, val := range hdr {
		if w.seenHeaders[key] {
			continue
		}
		key = strings.TrimPrefix(key, http.TrailerPrefix)
		// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-WEB.md#protocol-differences-vs-grpc-over-http2
		tr[strings.ToLower(key)] = val
	}

	var buf bytes.Buffer
	if err := tr.Write(&buf); err != nil {
		return err
	}

	head := []byte{1 << 7, 0, 0, 0, 0} // MSB=1 indicates this is a trailer data frame.
	binary.BigEndian.PutUint32(head[1:5], uint32(buf.Len()))
	if _, err := w.ResponseWriter.Write(head); err != nil {
		return err
	}
	if _, err := w.ResponseWriter.Write(buf.Bytes()); err != nil {
		return err
	}
	return nil
}

// base64ResponseWriter enc
type base64ResponseWriter struct {
	http.ResponseWriter
	encoder io.WriteCloser
}

func newBase64ResponseWriter(w http.ResponseWriter) *base64ResponseWriter {
	return &base64ResponseWriter{
		ResponseWriter: w,
		encoder:        base64.NewEncoder(base64.StdEncoding, w),
	}
}

func (w *base64ResponseWriter) Write(b []byte) (int, error) {
	return w.encoder.Write(b)
}
func (w *base64ResponseWriter) Flush() {
	if err := w.encoder.Close(); err != nil {
		panic(err)
	}
	w.encoder = base64.NewEncoder(base64.StdEncoding, w.ResponseWriter)
	flushWriter(w.ResponseWriter)
}

// prefixReader
type prefixReader struct {
	io.ReadCloser
	buf        bytes.Buffer
	buffered   bool
	compressed bool
}

func (r *prefixReader) Read(b []byte) (int, error) {
	if r.buffered {
		return r.buf.Read(b)
	}

	// TODO: limit size.
	body, err := io.ReadAll(r.ReadCloser)
	if err != nil {
		return 0, err
	}
	size := uint32(len(body))

	prefix := [5]byte{}
	prefix[0] = 0 // uncompressed
	if r.compressed {
		prefix[0] = 1 // compressed
	}

	binary.BigEndian.PutUint32(prefix[1:], size)
	r.buf.Write(prefix[:]) //nolint
	r.buf.Write(body)      //nolint
	r.buffered = true
	return r.buf.Read(b)
}

func (r *prefixReader) Close() error {
	return r.ReadCloser.Close()
}

var (
	isConnectHeader = map[string]bool{
		connectUnaryHeaderCompression:           true,
		connectUnaryHeaderAcceptCompression:     true,
		connectStreamingHeaderCompression:       true,
		connectStreamingHeaderAcceptCompression: true,
		connectHeaderTimeout:                    true,
		connectHeaderProtocolVersion:            true,
	}
	isGRPCHeader = map[string]bool{
		grpcHeaderCompression:       true,
		grpcHeaderAcceptCompression: true,
		grpcHeaderTimeout:           true,
		grpcHeaderStatus:            true,
		grpcHeaderMessage:           true,
		grpcHeaderDetails:           true,
	}
)

func translateConnectToGRPC(r *http.Request, typ, enc string) bool {
	//TODO: r.Method = http.MethodPost || http.MethodGet
	r.ProtoMajor = 2
	r.ProtoMinor = 0

	delHeaderCanonical(r.Header, connectHeaderProtocolVersion)
	delHeaderCanonical(r.Header, headerContentLength)

	if strings.HasPrefix(typ, connectStreamingContentTypeDefault) {
		// stream
		ct := grpcContentTypePrefix + enc
		setHeaderCanonical(r.Header, headerContentType, ct)
		if x := getHeaderCanonical(r.Header, connectStreamingHeaderCompression); len(x) > 0 {
			setHeaderCanonical(r.Header, grpcHeaderCompression, x)
			delHeaderCanonical(r.Header, connectStreamingHeaderCompression)
		}
		if x := getHeaderCanonical(r.Header, connectStreamingHeaderAcceptCompression); len(x) > 0 {
			setHeaderCanonical(r.Header, grpcHeaderAcceptCompression, x)
			delHeaderCanonical(r.Header, connectStreamingHeaderAcceptCompression)
		}

	} else {
		// unary
		ct := grpcContentTypePrefix + strings.TrimPrefix(typ, "application/")
		setHeaderCanonical(r.Header, headerContentType, ct)

		compressed := false
		if x := getHeaderCanonical(r.Header, connectUnaryHeaderCompression); len(x) > 0 {
			compressed = x != "identity"
			setHeaderCanonical(r.Header, grpcHeaderCompression, x)
			delHeaderCanonical(r.Header, connectUnaryHeaderCompression)
		}
		if x := getHeaderCanonical(r.Header, connectUnaryHeaderAcceptCompression); len(x) > 0 {
			setHeaderCanonical(r.Header, grpcHeaderAcceptCompression, x)
			delHeaderCanonical(r.Header, connectUnaryHeaderAcceptCompression)
		}

		r.Body = &prefixReader{
			ReadCloser: r.Body,
			compressed: compressed,
		}
	}

	if to := getHeaderCanonical(r.Header, connectHeaderTimeout); len(to) > 0 {
		delHeaderCanonical(r.Header, connectHeaderTimeout)
		setHeaderCanonical(r.Header, grpcHeaderTimeout, to)
	}

	return true
}

type connectResponseWriter struct {
	http.ResponseWriter
	typ, enc string

	statusCode  int
	body        bytes.Buffer // buffered body for unary payloads
	header      http.Header  // buffered header for trailer capture
	wroteHeader bool
	wrotePrefix int // unary prefix
}

func newConnectResponseWriter(w http.ResponseWriter, typ, enc string) *connectResponseWriter {
	return &connectResponseWriter{
		ResponseWriter: w,
		typ:            typ,
		enc:            enc,
	}
}

func (w *connectResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *connectResponseWriter) isStreaming() bool {
	// isStreaming presumes same typ for request to response.
	return strings.HasPrefix(w.typ, connectStreamingContentTypeDefault)
}

func (w *connectResponseWriter) writeHeader() {
	// Encode header response.

	// TODO: headers...
	//ct := getHeaderCanonical(r.Header, headerContentType)
	hdr := w.ResponseWriter.Header()

	if w.isStreaming() {
		// stream
		ct := connectStreamingContentTypePrefix + w.enc
		setHeaderCanonical(hdr, headerContentType, ct)

		setHeaderCanonical(
			hdr, connectStreamingHeaderCompression,
			getHeaderCanonical(w.header, grpcHeaderCompression),
		)

	} else {
		// unary

		ct := "application/" + w.enc
		setHeaderCanonical(hdr, headerContentType, ct)

		setHeaderCanonical(
			hdr, connectUnaryHeaderCompression,
			getHeaderCanonical(w.header, grpcHeaderCompression),
		)
	}

	for k, v := range w.header {

		key := textproto.CanonicalMIMEHeaderKey(k)
		isTrailer := strings.HasPrefix(k, http.TrailerPrefix)
		if isTrailer {
			key = key[len(http.TrailerPrefix):]
		}
		if isGRPCHeader[key] || isConnectHeader[key] {
			continue
		}
		if isTrailer {
			key = connectUnaryTrailerPrefix + key
		}
		hdr[key] = v
	}

	w.wroteHeader = true
}

func (w *connectResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	if w.isStreaming() {
		if !w.wroteHeader {
			w.writeHeader()
		}
		w.ResponseWriter.WriteHeader(statusCode)
	}
}

func (w *connectResponseWriter) Write(b []byte) (int, error) {
	if w.isStreaming() {
		if !w.wroteHeader {
			w.writeHeader()
		}
		return w.ResponseWriter.Write(b)
	}

	prefixSize := 5 - w.wrotePrefix
	if prefixSize <= 0 {
		return w.body.Write(b)
	}
	size := len(b)
	if size < prefixSize {
		prefixSize = size
	}
	w.wrotePrefix += prefixSize
	b = b[prefixSize:]
	return w.body.Write(b)
}

func (w *connectResponseWriter) finalize() error {
	if !w.wroteHeader {
		w.writeHeader()
	}
	if !w.isStreaming() {
		// Trailers supported only in headers.
		if _, err := w.body.WriteTo(w.ResponseWriter); err != nil {
			return err
		}
	}
	flushWriter(w.ResponseWriter)
	return nil
}

func (w *connectResponseWriter) Flush() {
	if w.isStreaming() {
		flushWriter(w.ResponseWriter)
	}
}

// HandleToGRPC translates connect and gRPC-web to a gRPC request for
// use with GRPC handlers.
func HandleToGRPC(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		typ, enc, ok := parseTypeEncoding(r.Header)
		if !ok {
			// Let handler serve error response.
			h.ServeHTTP(w, r)
			return
		}
		switch typ {
		case grpcContentTypeDefault:
			// grpc -> grpc
			h.ServeHTTP(w, r)
		case grpcWebContentTypeDefault:
			// grpc-web -> grpc
			if !translateGRPCWebToGRPC(r, typ, enc) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			ww := newGRPCWebResponseWriter(w, typ, enc)
			h.ServeHTTP(ww, r)
			ww.flushWithTrailers()
		default:
			// connect -> grpc
			if !translateConnectToGRPC(r, typ, enc) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			ww := newConnectResponseWriter(w, typ, enc)
			h.ServeHTTP(ww, r)
			if err := ww.finalize(); err != nil {
				panic(err) // TODO
			}
		}
	})
}

func flushWriter(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
