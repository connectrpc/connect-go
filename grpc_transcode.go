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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/textproto"
	"strings"
)

type adapterConfig struct {
	Codec        Codec
	BufferPool   *bufferPool
	ReadMaxBytes int
	SendMaxBytes int
}

func newAdapterConfig(options []AdapterOption) *adapterConfig {
	config := &adapterConfig{
		Codec:      &protoBinaryCodec{},
		BufferPool: newBufferPool(),
	}
	for _, option := range options {
		option.applyToAdapter(config)
	}
	return config
}

// ensureTeTrailers ensures that the "Te" header contains "trailers".
func ensureTeTrailers(header http.Header) {
	teValues := header["Te"]
	for _, val := range teValues {
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
	header["Te"] = append(teValues, "trailers")
}

func translateGRPCWebToGRPC(request *http.Request, enc string) {
	request.ProtoMajor = 2
	request.ProtoMinor = 0
	header := request.Header
	ensureTeTrailers(header)

	delHeaderCanonical(header, headerContentLength)
	contentType := grpcContentTypePrefix + enc
	setHeaderCanonical(header, headerContentType, contentType)
}

type grpcWebResponseWriter struct {
	http.ResponseWriter
	typ, enc string

	seenHeaders map[string]bool
	wroteHeader bool
}

func newGRPCWebResponseWriter(responseWriter http.ResponseWriter, typ, enc string) *grpcWebResponseWriter {
	return &grpcWebResponseWriter{
		ResponseWriter: responseWriter,
		typ:            typ,
		enc:            enc,
	}
}

func (w *grpcWebResponseWriter) writeHeaders() {
	header := w.Header()

	isTrailer := map[string]bool{
		// Force GRPC status values to be trailers.
		grpcHeaderStatus:  true,
		grpcHeaderDetails: true,
		grpcHeaderMessage: true,
	}
	for _, key := range strings.Split(header.Get(headerTrailer), ",") {
		key = http.CanonicalHeaderKey(key)
		isTrailer[key] = true
	}

	keys := make(map[string]bool, len(header))
	for key := range header {
		if strings.HasPrefix(key, http.TrailerPrefix) {
			continue
		}
		canonicalKey := http.CanonicalHeaderKey(key)
		if key != canonicalKey {
			header[canonicalKey] = header[key]
			delete(header, key)
			key = canonicalKey
		}
		if isTrailer[key] {
			continue
		}
		keys[key] = true
	}

	setHeaderCanonical(header, headerContentType, w.typ+"+"+w.enc)

	w.seenHeaders = keys
	w.wroteHeader = true
}

func (w *grpcWebResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.writeHeaders()
	}
	return w.ResponseWriter.Write(b)
}

func (w *grpcWebResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.writeHeaders()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *grpcWebResponseWriter) flushWithTrailers() {
	if !w.wroteHeader {
		w.writeHeaders()
	}
	if err := w.writeTrailer(); err != nil {
		return // nothing
	}
	flushResponseWriter(w.ResponseWriter)
}

func (w *grpcWebResponseWriter) Flush() {
	flushResponseWriter(w.ResponseWriter)
}

func (w *grpcWebResponseWriter) writeTrailer() error {
	header := w.Header()
	trailer := make(http.Header, len(header)-len(w.seenHeaders))
	for key, values := range header {
		if w.seenHeaders[key] {
			continue
		}
		key = strings.TrimPrefix(key, http.TrailerPrefix)
		// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-WEB.md#protocol-differences-vs-grpc-over-http2
		trailer[strings.ToLower(key)] = values
	}

	var buf bytes.Buffer
	if err := trailer.Write(&buf); err != nil {
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

// bufferedEnvelopeReader buffers a message to wrap it in an envelope.
type bufferedEnvelopeReader struct {
	io.ReadCloser

	config     *adapterConfig
	buf        bytes.Buffer
	buffered   bool
	compressed bool
	err        error
}

func (r *bufferedEnvelopeReader) fillBuffer() error {
	body, err := readAll(nil, r.ReadCloser, int64(r.config.ReadMaxBytes))
	if err != nil {
		// Limit reached if EOF.
		if errors.Is(err, io.EOF) {
			bytesRead := int64(len(body))
			discardedBytes, err := io.Copy(io.Discard, r.ReadCloser)
			if err != nil {
				return errorf(CodeResourceExhausted,
					"message is larger than configured max %d - unable to determine message size: %w",
					r.config.ReadMaxBytes, err)
			}
			return errorf(CodeResourceExhausted,
				"message size %d is larger than configured max %d",
				bytesRead+discardedBytes, r.config.ReadMaxBytes)
		}
		return errorf(CodeUnknown, "read message: %w", err)
	}
	size := uint32(len(body))

	var head [5]byte
	head[0] = 0 // uncompressed
	if r.compressed {
		head[0] = 1 // compressed
	}
	binary.BigEndian.PutUint32(head[1:], size)
	r.buf.Write(head[:])
	r.buf.Write(body)
	return nil
}

func (r *bufferedEnvelopeReader) Read(data []byte) (int, error) {
	if !r.buffered && r.ReadCloser != nil {
		r.err = r.fillBuffer()
	}
	r.buffered = true
	if r.err != nil {
		return 0, r.err
	}
	return r.buf.Read(data)
}

// readAll reads from r until an error or the limit is reached.
func readAll(data []byte, reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = math.MaxInt32
	}
	// Copied from io.ReadAll with the limit applied.
	var total int64
	for {
		if len(data) == cap(data) {
			// Add more capacity (let append pick how much).
			data = append(data, 0)[:len(data)]
		}
		n, err := reader.Read(data[len(data):cap(data)])
		data = data[:len(data)+n]
		total += int64(n)
		if total > limit {
			return data, io.EOF
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return data, err
		}
	}
}

func (r *bufferedEnvelopeReader) Close() error {
	if r.ReadCloser != nil {
		return r.ReadCloser.Close()
	}
	return nil
}

func translateConnectToGRPC(request *http.Request, typ, enc string, config *adapterConfig) {
	request.ProtoMajor = 2
	request.ProtoMinor = 0
	header := request.Header
	ensureTeTrailers(header)

	delHeaderCanonical(header, connectHeaderProtocolVersion)
	delHeaderCanonical(header, headerContentLength)

	// Translate timeout header
	if timeout := getHeaderCanonical(header, connectHeaderTimeout); len(timeout) > 0 {
		delHeaderCanonical(header, connectHeaderTimeout)
		setHeaderCanonical(header, grpcHeaderTimeout, timeout+"m")
	}

	// stream
	if strings.HasPrefix(typ, connectStreamingContentTypeDefault) {
		contentType := grpcContentTypePrefix + enc
		setHeaderCanonical(header, headerContentType, contentType)
		if contentEncoding := getHeaderCanonical(header, connectStreamingHeaderCompression); len(contentEncoding) > 0 {
			delHeaderCanonical(header, connectStreamingHeaderCompression)
			setHeaderCanonical(header, grpcHeaderCompression, contentEncoding)
		}
		if acceptEncoding := getHeaderCanonical(header, connectStreamingHeaderAcceptCompression); len(acceptEncoding) > 0 {
			delHeaderCanonical(header, connectStreamingHeaderAcceptCompression)
			setHeaderCanonical(header, grpcHeaderAcceptCompression, acceptEncoding)
		}
		return
	}

	// unary
	contentType := grpcContentTypePrefix + strings.TrimPrefix(typ, "application/")
	setHeaderCanonical(header, headerContentType, contentType)

	if acceptEncoding := getHeaderCanonical(header, connectUnaryHeaderAcceptCompression); len(acceptEncoding) > 0 {
		delHeaderCanonical(header, connectUnaryHeaderAcceptCompression)
		setHeaderCanonical(header, grpcHeaderAcceptCompression, acceptEncoding)
	}

	generateGetBody := func() {
		request.Method = http.MethodPost
		query := request.URL.Query()
		request.URL.RawQuery = "" // clear query parameters

		compressed := false
		if contentEncoding := query.Get(connectUnaryCompressionQueryParameter); len(contentEncoding) > 0 {
			compressed = contentEncoding != "identity"
			delHeaderCanonical(header, connectUnaryHeaderCompression)
			setHeaderCanonical(header, grpcHeaderCompression, contentEncoding)
		}

		msg := query.Get(connectUnaryMessageQueryParameter)
		msgReader := queryValueReader(msg, query.Get(connectUnaryBase64QueryParameter) == "1")
		request.Body = &bufferedEnvelopeReader{
			ReadCloser: io.NopCloser(msgReader),
			compressed: compressed,
			config:     config,
		}
	}
	generatePostBody := func() {
		compressed := false
		if contentEncoding := getHeaderCanonical(header, connectUnaryHeaderCompression); len(contentEncoding) > 0 {
			compressed = contentEncoding != "identity"
			delHeaderCanonical(header, connectUnaryHeaderCompression)
			setHeaderCanonical(header, grpcHeaderCompression, contentEncoding)
		}
		request.Body = &bufferedEnvelopeReader{
			ReadCloser: request.Body,
			compressed: compressed,
			config:     config,
		}
	}

	// Handle GET request with query parameters
	if request.Method == http.MethodGet {
		generateGetBody()
	} else {
		generatePostBody()
	}
}

func isProtocolHeader(header string) bool {
	return header == connectUnaryHeaderCompression ||
		header == connectUnaryHeaderAcceptCompression ||
		header == connectStreamingHeaderCompression ||
		header == connectStreamingHeaderAcceptCompression ||
		header == connectHeaderTimeout ||
		header == connectHeaderProtocolVersion ||
		header == grpcHeaderCompression ||
		header == grpcHeaderAcceptCompression ||
		header == grpcHeaderTimeout ||
		header == grpcHeaderStatus ||
		header == grpcHeaderMessage ||
		header == grpcHeaderDetails
}

type connectResponseWriter struct {
	http.ResponseWriter
	typ, enc string
	config   *adapterConfig

	body        bytes.Buffer // buffered body for unary payloads
	header      http.Header  // buffered header for trailer capture
	wroteHeader bool
	wroteHead   int // unary envelope head
}

func newConnectResponseWriter(responseWriter http.ResponseWriter, typ, enc string, config *adapterConfig) *connectResponseWriter {
	return &connectResponseWriter{
		ResponseWriter: responseWriter,
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

	for rawKey, values := range w.header {
		key := textproto.CanonicalMIMEHeaderKey(rawKey)
		isTrailer := strings.HasPrefix(key, http.TrailerPrefix)
		if isProtocolHeader(key) || isTrailer {
			continue
		}
		if rawKey != key {
			delete(header, rawKey)
		}
		header[key] = values
	}
	w.wroteHeader = true
}

func (w *connectResponseWriter) WriteHeader(statusCode int) {
	if w.isStreaming() {
		if !w.wroteHeader {
			w.writeHeader()
		}
		w.ResponseWriter.WriteHeader(statusCode)
	}
}

func (w *connectResponseWriter) writeBuffered(data []byte) (int, error) {
	headSize := 5 - w.wroteHead
	if headSize <= 0 {
		return w.body.Write(data)
	}

	size := len(data)
	if size < headSize {
		headSize = size
	}
	w.wroteHead += headSize
	data = data[headSize:]
	return w.body.Write(data)
}

func (w *connectResponseWriter) Write(data []byte) (int, error) {
	if w.isStreaming() {
		if !w.wroteHeader {
			w.writeHeader()
		}
		return w.ResponseWriter.Write(data)
	}

	wroteN, err := w.writeBuffered(data)
	total := w.body.Len() + w.wroteHead
	limit := w.config.SendMaxBytes
	if limit > 0 && total > limit {
		return 0, NewError(CodeResourceExhausted, fmt.Errorf("message size %d exceeds sendMaxBytes %d", total, limit))
	}
	return wroteN, err
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
		if strings.HasPrefix(key, http.TrailerPrefix) {
			// Must remove trailer prefix before canonicalizing.
			key = http.CanonicalHeaderKey(key[len(http.TrailerPrefix):])
			trailer[key] = vals
		} else if key := http.CanonicalHeaderKey(key); isTrailer[key] {
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

	protobuf := w.config.Codec
	trailerErr := grpcErrorFromTrailer(w.config.BufferPool, protobuf, trailer)
	if trailerErr != nil {
		if w.isStreaming() {
			trailerErr.meta = trailer
		}
		return trailerErr
	}

	// Remove all protocol trailer keys.
	for key := range trailer {
		if isProtocolHeader(key) {
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
			return err
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
		if isProtocolHeader(key) {
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
	// Flush is a no-op for unary requests.
	if w.isStreaming() {
		flushResponseWriter(w.ResponseWriter)
	}
}

// NewGRPCAdapter returns a new http.Handler that transcodes connect and
// gRPC-Web requests to gRPC.
//
// The adapter supports unary and streaming requests and responses but does not
// validate the transport can support bidi streaming. Connect unary requests and
// responses are buffered in memory and subject to the configured limits.
// Connect streaming requests and responses are not buffered and defer to
// the handlers limits.
//
// To use the adapter, pass it an http.Handler that serves gRPC requests such as:
//
//	grpcServer := grpc.NewServer()
//	mux := connect.NewGRPCAdapter(grpcServer,
//		connect.WithReadMaxBytes(1024),
//		connect.WithWriteMaxBytes(1024),
//	)
//	http.ListenAndServe(":8080", h2c.NewHandler(mux, &http2.Server{}))
func NewGRPCAdapter(grpcHandler http.Handler, options ...AdapterOption) http.Handler {
	errorWriter := NewErrorWriter()
	config := newAdapterConfig(options)

	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		method := request.Method
		header := request.Header
		contentType := canonicalizeContentType(getHeaderCanonical(header, headerContentType))

		// parse content-type into type and encoding
		typ, enc, found := strings.Cut(contentType, "+")
		if !found {
			enc = "proto"
		}
		isHandledType := typ == grpcContentTypeDefault && method == http.MethodPost ||
			typ == grpcWebContentTypeDefault && method == http.MethodPost ||
			typ == connectStreamingContentTypeDefault && method == http.MethodPost || // connect streaming
			strings.HasPrefix(typ, connectUnaryContentTypePrefix) &&
				(method == http.MethodPost || method == http.MethodGet) // connect unary

		if !isHandledType {
			// Let the handler serve the error response.
			grpcHandler.ServeHTTP(responseWriter, request)
			return
		}
		switch typ {
		case grpcContentTypeDefault:
			// grpc -> grpc
			grpcHandler.ServeHTTP(responseWriter, request)
		case grpcWebContentTypeDefault:
			// grpc-web -> grpc
			translateGRPCWebToGRPC(request, enc)
			ww := newGRPCWebResponseWriter(responseWriter, typ, enc)
			grpcHandler.ServeHTTP(ww, request)
			ww.flushWithTrailers()
		default:
			// connect -> grpc
			translateConnectToGRPC(request, typ, enc, config)
			ww := newConnectResponseWriter(responseWriter, typ, enc, config)
			grpcHandler.ServeHTTP(ww, request)
			if err := ww.finalize(); err != nil {
				responseHeader := responseWriter.Header()
				if ww.isStreaming() {
					setHeaderCanonical(responseHeader, headerContentType, typ)
					_ = errorWriter.writeConnectStreaming(responseWriter, err)
				} else {
					delHeaderCanonical(responseHeader, connectUnaryHeaderCompression)
					setHeaderCanonical(responseHeader, headerContentType, connectUnaryContentTypeJSON)
					_ = errorWriter.writeConnectUnary(responseWriter, err)
				}
			}
		}
	})
}
