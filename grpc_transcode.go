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
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/textproto"
	"strings"
)

// parseTypeEncoding parses the type and encoding from the content-type header.
func parseTypeEncoding(method string, header http.Header) (typ, enc string, ok bool) {
	contentType := canonicalizeContentType(getHeaderCanonical(header, headerContentType))

	typ, enc, found := strings.Cut(contentType, "+")
	if !found {
		enc = "proto"
	}
	ok = typ == grpcContentTypeDefault && method == http.MethodPost ||
		typ == grpcWebContentTypeDefault && method == http.MethodPost ||
		typ == grpcWebTextContentTypeDefault && method == http.MethodPost ||
		typ == connectStreamingContentTypeDefault && method == http.MethodPost || // connect streaming
		strings.HasPrefix(typ, connectUnaryContentTypePrefix) &&
			(method == http.MethodPost || method == http.MethodGet) // connect unary

	return typ, enc, ok
}

type readCloser struct {
	io.Reader
	io.Closer
}

// ensureTeTrailers ensures that the "Te" header contains "trailers".
func ensureTeTrailers(h http.Header) {
	te := h["Te"]
	for _, val := range te {
		valElements := strings.Split(val, ",")
		for _, element := range valElements {
			pos := strings.IndexByte(element, ';')
			if pos != -1 {
				element = element[:pos]
			}
			if strings.ToLower(element) == "trailers" {
				return // "trailers" is already present
			}
		}
	}
	h["Te"] = append(te, "trailers")
}

func translateGRPCWebToGRPC(r *http.Request, typ, enc string) {
	r.ProtoMajor = 2
	r.ProtoMinor = 0
	ensureTeTrailers(r.Header)

	delHeaderCanonical(r.Header, headerContentLength)
	contentType := grpcContentTypePrefix + enc
	setHeaderCanonical(r.Header, headerContentType, contentType)

	if typ == grpcWebTextContentTypeDefault {
		body := base64.NewDecoder(base64.StdEncoding, r.Body)
		r.Body = readCloser{body, r.Body}
	}
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
	flushResponseWriter(w.ResponseWriter)
}

func (w *grpcWebResponseWriter) Flush() {
	flushResponseWriter(w.ResponseWriter)
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

	head := [5]byte{1 << 7, 0, 0, 0, 0} // MSB=1 indicates this is a trailer data frame.
	binary.BigEndian.PutUint32(head[1:5], uint32(buf.Len()))
	if _, err := w.ResponseWriter.Write(head[:]); err != nil {
		return err
	}
	if _, err := w.ResponseWriter.Write(buf.Bytes()); err != nil {
		return err
	}
	return nil
}

// base64ResponseWriter encodes the response body in base64.
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
	flushResponseWriter(w.ResponseWriter)
}

// bufferedEnvelopeReader buffers a message to wrap it in an envelope.
type bufferedEnvelopeReader struct {
	io.ReadCloser

	config     *handlerConfig
	buf        bytes.Buffer
	buffered   bool
	compressed bool
	err        error
}

func (r *bufferedEnvelopeReader) Read(b []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.buffered {
		return r.buf.Read(b)
	}

	body, err := readAll(nil, r.ReadCloser, int64(r.config.ReadMaxBytes))
	if err != nil {
		// Limit reached if EOF.
		if err == io.EOF {
			bytesRead := int64(len(body))
			discardedBytes, err := io.Copy(io.Discard, r.ReadCloser)
			if err != nil {
				r.err = errorf(CodeResourceExhausted,
					"message is larger than configured max %d - unable to determine message size: %w",
					r.config.ReadMaxBytes, err)
				return 0, r.err
			}
			r.err = errorf(CodeResourceExhausted,
				"message size %d is larger than configured max %d",
				bytesRead+discardedBytes, r.config.ReadMaxBytes)
			return 0, r.err
		}
		return 0, errorf(CodeUnknown, "read message: %w", err)
	}
	size := uint32(len(body))

	var head [5]byte
	head[0] = 0 // uncompressed
	if r.compressed {
		head[0] = 1 // compressed
	}
	binary.BigEndian.PutUint32(head[1:], size)
	r.buf.Write(head[:]) //nolint
	r.buf.Write(body)    //nolint
	r.buffered = true
	return r.buf.Read(b)
}

// readAll reads from r until an error or the limit is reached.
func readAll(b []byte, r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = math.MaxInt32
	}
	// Copied from io.ReadAll with the limit applied.
	var total int64
	for {
		if len(b) == cap(b) {
			// Add more capacity (let append pick how much).
			b = append(b, 0)[:len(b)]
		}
		n, err := r.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		total += int64(n)
		if total > limit {
			return b, io.EOF
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return b, err
		}
	}
}

func (r *bufferedEnvelopeReader) Close() error {
	if r.ReadCloser != nil {
		return r.ReadCloser.Close()
	}
	return nil
}

func translateConnectToGRPC(r *http.Request, typ, enc string, config *handlerConfig) {
	r.ProtoMajor = 2
	r.ProtoMinor = 0
	ensureTeTrailers(r.Header)

	delHeaderCanonical(r.Header, connectHeaderProtocolVersion)
	delHeaderCanonical(r.Header, headerContentLength)

	// Translate timeout header
	if timeout := getHeaderCanonical(r.Header, connectHeaderTimeout); len(timeout) > 0 {
		delHeaderCanonical(r.Header, connectHeaderTimeout)
		setHeaderCanonical(r.Header, grpcHeaderTimeout, timeout+"m")
	}

	// stream
	if strings.HasPrefix(typ, connectStreamingContentTypeDefault) {
		contentType := grpcContentTypePrefix + enc
		setHeaderCanonical(r.Header, headerContentType, contentType)
		if contentEncoding := getHeaderCanonical(r.Header, connectStreamingHeaderCompression); len(contentEncoding) > 0 {
			delHeaderCanonical(r.Header, connectStreamingHeaderCompression)
			setHeaderCanonical(r.Header, grpcHeaderCompression, contentEncoding)
		}
		if acceptEncoding := getHeaderCanonical(r.Header, connectStreamingHeaderAcceptCompression); len(acceptEncoding) > 0 {
			delHeaderCanonical(r.Header, connectStreamingHeaderAcceptCompression)
			setHeaderCanonical(r.Header, grpcHeaderAcceptCompression, acceptEncoding)
		}
		return
	}

	// unary
	contentType := grpcContentTypePrefix + strings.TrimPrefix(typ, "application/")
	setHeaderCanonical(r.Header, headerContentType, contentType)

	if acceptEncoding := getHeaderCanonical(r.Header, connectUnaryHeaderAcceptCompression); len(acceptEncoding) > 0 {
		delHeaderCanonical(r.Header, connectUnaryHeaderAcceptCompression)
		setHeaderCanonical(r.Header, grpcHeaderAcceptCompression, acceptEncoding)
	}

	// Handle GET request with query parameters
	if r.Method == http.MethodGet {
		r.Method = http.MethodPost
		query := r.URL.Query()
		r.URL.RawQuery = "" // clear query parameters

		compressed := false
		if contentEncoding := query.Get(connectUnaryCompressionQueryParameter); len(contentEncoding) > 0 {
			compressed = contentEncoding != "identity"
			delHeaderCanonical(r.Header, connectUnaryHeaderCompression)
			setHeaderCanonical(r.Header, grpcHeaderCompression, contentEncoding)
		}

		var failed error
		version := query.Get(connectUnaryConnectQueryParameter)
		if version == "" && config.RequireConnectProtocolHeader {
			failed = errorf(CodeInvalidArgument, "missing required query parameter: set %s to %q", connectUnaryConnectQueryParameter, connectUnaryConnectQueryValue)
		} else if version != "" && version != connectUnaryConnectQueryValue {
			failed = errorf(CodeInvalidArgument, "%s must be %q: got %q", connectUnaryConnectQueryParameter, connectUnaryConnectQueryValue, version)
		}
		if failed != nil {
			r.Body = &bufferedEnvelopeReader{err: failed}
			return
		}
		msg := query.Get(connectUnaryMessageQueryParameter)
		msgReader := queryValueReader(msg, query.Get(connectUnaryBase64QueryParameter) == "1")
		r.Body = &bufferedEnvelopeReader{
			ReadCloser: io.NopCloser(msgReader),
			compressed: compressed,
			config:     config,
		}

	} else {
		compressed := false
		if contentEncoding := getHeaderCanonical(r.Header, connectUnaryHeaderCompression); len(contentEncoding) > 0 {
			compressed = contentEncoding != "identity"
			delHeaderCanonical(r.Header, connectUnaryHeaderCompression)
			setHeaderCanonical(r.Header, grpcHeaderCompression, contentEncoding)
		}
		r.Body = &bufferedEnvelopeReader{
			ReadCloser: r.Body,
			compressed: compressed,
			config:     config,
		}
	}
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

type connectResponseWriter struct {
	http.ResponseWriter
	typ, enc string
	config   *handlerConfig

	statusCode  int
	body        bytes.Buffer // buffered body for unary payloads
	header      http.Header  // buffered header for trailer capture
	wroteHeader bool
	wrotePrefix int // unary prefix
}

func newConnectResponseWriter(w http.ResponseWriter, typ, enc string, config *handlerConfig) *connectResponseWriter {
	return &connectResponseWriter{
		ResponseWriter: w,
		typ:            typ,
		enc:            enc,
		config:         config,
	}
}

func (w *connectResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

// isStreaming checks via the content type if the response is streaming.
func (w *connectResponseWriter) isStreaming() bool {
	return w.typ == connectStreamingContentTypeDefault
}

func (w *connectResponseWriter) writeHeader() {
	// Encode header response.
	header := w.ResponseWriter.Header()
	compression := getHeaderCanonical(w.header, grpcHeaderCompression)

	if w.isStreaming() {
		contentType := connectStreamingContentTypePrefix + w.enc
		setHeaderCanonical(header, headerContentType, contentType)
		setHeaderCanonical(header, connectStreamingHeaderCompression, compression)

	} else {
		contentType := w.typ // type has no encoding suffix
		setHeaderCanonical(header, headerContentType, contentType)
		setHeaderCanonical(header, connectUnaryHeaderCompression, compression)
	}

	for k, v := range w.header {
		key := textproto.CanonicalMIMEHeaderKey(k)
		isTrailer := strings.HasPrefix(key, http.TrailerPrefix)
		if isGRPCHeader[key] || isConnectHeader[key] || isTrailer {
			continue
		}
		header[key] = v
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

func (w *connectResponseWriter) Write(b []byte) (n int, err error) {
	if w.isStreaming() {
		if !w.wroteHeader {
			w.writeHeader()
		}
		return w.ResponseWriter.Write(b)
	}

	prefixSize := 5 - w.wrotePrefix
	if prefixSize <= 0 {
		n, err = w.body.Write(b)
	} else {
		size := len(b)
		if size < prefixSize {
			prefixSize = size
		}
		w.wrotePrefix += prefixSize
		b = b[prefixSize:]
		n, err = w.body.Write(b)
	}
	total := w.body.Len() + w.wrotePrefix
	limit := w.config.SendMaxBytes
	if limit > 0 && total > limit {
		return n, NewError(CodeResourceExhausted, fmt.Errorf("message size %d exceeds sendMaxBytes %d", total, limit))
	}
	return n, err
}

func getGRPCTrailer(header http.Header) http.Header {
	isTrailer := map[string]bool{
		// Force GRPC status values, try copy them directly from header.
		grpcHeaderStatus:  true,
		grpcHeaderDetails: true,
		grpcHeaderMessage: true,
	}
	for _, key := range strings.Split(header.Get(headerTrailer), ",") {
		key = http.CanonicalHeaderKey(key)
		isTrailer[key] = true
	}

	trailer := make(http.Header)
	for key, vals := range header {
		key = http.CanonicalHeaderKey(key)
		if strings.HasPrefix(key, http.TrailerPrefix) || isTrailer[key] {
			key = strings.TrimPrefix(key, http.TrailerPrefix)
			trailer[key] = vals
		}
	}
	return trailer
}

func (w *connectResponseWriter) finalize() error {
	if !w.wroteHeader {
		w.writeHeader()
	}

	header := w.Header()
	trailer := getGRPCTrailer(header)

	protobuf := w.config.Codecs[codecNameProto]
	trailerErr := grpcErrorFromTrailer(w.config.BufferPool, protobuf, trailer)
	if trailerErr != nil {
		if w.isStreaming() {
			trailerErr.meta = trailer
		}
		return trailerErr
	}

	// Remove all gRPC trailer keys.
	for key := range trailer {
		if isGRPCHeader[key] {
			delete(trailer, key)
		}
	}

	if w.isStreaming() {
		// Encode as connect end message.
		end := &connectEndStreamMessage{
			Trailer: trailer,
		}
		data, err := json.Marshal(end)
		if err != nil {
			panic(err)
		}

		head := [5]byte{connectFlagEnvelopeEndStream, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(head[1:5], uint32(len(data)))
		if _, err := w.ResponseWriter.Write(head[:]); err != nil {
			return err
		}
		if _, err := w.ResponseWriter.Write(data); err != nil {
			return err
		}
		return nil
	}

	// Encode trailers as header
	responseHeader := w.ResponseWriter.Header()
	for key, vals := range trailer {
		if isGRPCHeader[key] || isConnectHeader[key] {
			continue
		}
		key = connectUnaryTrailerPrefix + key
		responseHeader[key] = vals
	}
	// Write buffered unary body to stream.
	if _, err := w.body.WriteTo(w.ResponseWriter); err != nil {
		return err
	}
	flushResponseWriter(w.ResponseWriter)
	return nil
}

func (w *connectResponseWriter) Flush() {
	if w.isStreaming() {
		flushResponseWriter(w.ResponseWriter)
	}
}

// GRPCHandler translates connect and gRPC-web to a gRPC request for
// use with a gRPC handler.
func GRPCHandler(h http.Handler, options ...HandlerOption) http.Handler {
	errorWriter := NewErrorWriter()
	config := newHandlerConfig("", options)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		typ, enc, ok := parseTypeEncoding(r.Method, r.Header)
		if !ok {
			// Let the handler serve the error response.
			h.ServeHTTP(w, r)
			return
		}
		switch typ {
		case grpcContentTypeDefault:
			// grpc -> grpc
			h.ServeHTTP(w, r)
		case grpcWebContentTypeDefault:
			// grpc-web -> grpc
			translateGRPCWebToGRPC(r, typ, enc)
			ww := newGRPCWebResponseWriter(w, typ, enc)
			h.ServeHTTP(ww, r)
			ww.flushWithTrailers()
		default:
			// connect -> grpc
			translateConnectToGRPC(r, typ, enc, config)
			ww := newConnectResponseWriter(w, typ, enc, config)
			h.ServeHTTP(ww, r)
			if err := ww.finalize(); err != nil {
				if ww.isStreaming() {
					setHeaderCanonical(w.Header(), headerContentType, typ)
					errorWriter.writeConnectStreaming(w, err) // ignore error
				} else {
					delHeaderCanonical(w.Header(), connectUnaryHeaderCompression)
					setHeaderCanonical(w.Header(), headerContentType, connectUnaryContentTypeJSON)
					errorWriter.writeConnectUnary(w, err) // ignore error
				}
			}
		}
	})
}
