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
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

// NewGRPCAdapter returns a new http.Handler that transcodes connect and
// gRPC-Web requests to gRPC.
//
// The adapter supports unary and streaming requests and responses but does not
// validate the transport can support bidi streaming. Connect unary requests and
// responses are buffered in memory and subject to the configured limits.
// Other protocols and connect streaming are not buffered and defer to the
// handlers limits.
//
// To use the adapter, pass it an http.Handler that serves gRPC requests such as:
//
//	grpcServer := grpc.NewServer()
//	mux := connect.NewGRPCAdapter(grpcServer,
//		connect.WithGRPCAdapterReadMaxBuffer(1024*1024),
//		connect.WithGRPCAdapterWriteMaxBuffer(1024*1024),
//	)
//	http.ListenAndServe(":8080", h2c.NewHandler(mux, &http2.Server{}))
func NewGRPCAdapter(grpcHandler http.Handler, options ...GRPCAdapterOption) http.Handler {
	config := newGRPCAdapterConfig(options)

	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		method := request.Method
		header := request.Header
		contentType := canonicalizeContentType(getHeaderCanonical(header, headerContentType))

		if _, ok := responseWriter.(http.Flusher); !ok {
			msg := "gRPC requires a ResponseWriter supporting http.Flusher"
			http.Error(responseWriter, msg, http.StatusInternalServerError)
			return
		}

		switch {
		case isGRPCContentType(method, contentType):
			// grpc -> grpc
			grpcHandler.ServeHTTP(responseWriter, request)
		case isGRPCWebContentType(method, contentType):
			// grpc-web -> grpc
			translateGRPCWebToGRPC(request, contentType)
			ww := newGRPCWebResponseWriter(responseWriter, contentType)
			grpcHandler.ServeHTTP(ww, request)
			ww.flushWithTrailers()
		case isConnectStreamingContentType(method, contentType):
			// connect streaming -> grpc
			translateConnectStreamingToGRPC(request, contentType)
			ww := newConnectStreamingResponseWriter(responseWriter, contentType, config)
			grpcHandler.ServeHTTP(ww, request)
			ww.flushWithTrailers()
		case isConnectUnaryContentType(method, contentType):
			// connect unary -> grpc
			translateConnectUnaryToGRPC(request, contentType, config.ReadMaxBytes)
			ww := newConnectUnaryResponseWriter(responseWriter, contentType, config)
			grpcHandler.ServeHTTP(ww, request)
			ww.flushWithTrailers()
		default:
			// unknown -> grpc
			grpcHandler.ServeHTTP(responseWriter, request)
		}
	})
}

// A GRRPCAdapterOption configures [NewGRPCAdapter].
type GRPCAdapterOption interface {
	applyToGRPCAdapter(*grpcAdapterConfig)
}

type grpcAdapterOptionFunc func(*grpcAdapterConfig)

func (f grpcAdapterOptionFunc) applyToGRPCAdapter(config *grpcAdapterConfig) {
	f(config)
}

// WithGRPCAdapterReadMaxBuffer returns a new AdapterOption that sets the
// maximum number of bytes that can be buffered for a connect unary request.
func WithGRPCAdapterReadMaxBuffer(readMaxBytes int) GRPCAdapterOption {
	return grpcAdapterOptionFunc(func(config *grpcAdapterConfig) {
		config.ReadMaxBytes = readMaxBytes
	})
}

// WithGRPCAdapterWriteMaxBuffer returns a new GRPCAdapterOption that sets the
// maximum number of bytes that can be buffered for a connect unary response.
func WithGRPCAdapterWriteMaxBuffer(writeMaxBytes int) GRPCAdapterOption {
	return grpcAdapterOptionFunc(func(config *grpcAdapterConfig) {
		config.WriteMaxBytes = writeMaxBytes
	})
}

type grpcAdapterConfig struct {
	BufferPool    *bufferPool
	ReadMaxBytes  int
	WriteMaxBytes int
	ErrorWriter   *ErrorWriter
}

func newGRPCAdapterConfig(options []GRPCAdapterOption) *grpcAdapterConfig {
	config := &grpcAdapterConfig{
		BufferPool:  newBufferPool(),
		ErrorWriter: NewErrorWriter(),
	}
	for _, option := range options {
		option.applyToGRPCAdapter(config)
	}
	return config
}

func isGRPCContentType(method, contentType string) bool {
	return method == http.MethodPost &&
		(contentType == grpcContentTypeDefault ||
			strings.HasPrefix(contentType, grpcContentTypePrefix))
}
func isGRPCWebContentType(method, contentType string) bool {
	return method == http.MethodPost &&
		(contentType == grpcWebContentTypeDefault ||
			strings.HasPrefix(contentType, grpcWebContentTypeDefault))
}
func isConnectStreamingContentType(method, contentType string) bool {
	return method == http.MethodPost &&
		(contentType == connectStreamingContentTypeDefault ||
			strings.HasPrefix(contentType, connectStreamingContentTypePrefix))
}
func isConnectUnaryContentType(method, contentType string) bool {
	return (method == http.MethodPost || method == http.MethodGet) &&
		strings.HasPrefix(contentType, connectUnaryContentTypePrefix) &&
		!strings.HasPrefix(contentType, connectStreamingContentTypePrefix) &&
		!strings.HasPrefix(contentType, grpcContentTypeDefault) &&
		!strings.HasPrefix(contentType, grpcWebContentTypeDefault)
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

func getTypeEncoding(contentType string) string {
	if i := strings.LastIndex(contentType, "+"); i >= 0 {
		return contentType[i+1:]
	}
	return "proto"
}

func translateGRPCWebToGRPC(request *http.Request, contentType string) {
	request.ProtoMajor = 2
	request.ProtoMinor = 0
	header := request.Header
	ensureTeTrailers(header)

	delHeaderCanonical(header, headerContentLength)
	grpcContentType := grpcContentTypePrefix + getTypeEncoding(contentType)
	setHeaderCanonical(header, headerContentType, grpcContentType)
}

type grpcWebResponseWriter struct {
	http.ResponseWriter
	contentType string

	seenHeaders map[string]bool
	wroteHeader bool
}

func newGRPCWebResponseWriter(responseWriter http.ResponseWriter, contentType string) *grpcWebResponseWriter {
	return &grpcWebResponseWriter{
		ResponseWriter: responseWriter,
		contentType:    contentType,
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

	setHeaderCanonical(header, headerContentType, w.contentType)

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

func (w *grpcWebResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
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

	maxBytes       int
	buf            bytes.Buffer
	hasInitialized bool
	initializedErr error
	isCompressed   bool
}

func (r *bufferedEnvelopeReader) fillBuffer() error {
	var bodyReader io.Reader = r.ReadCloser
	if r.maxBytes > 0 {
		bodyReader = io.LimitReader(bodyReader, int64(r.maxBytes)+1)
	}
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		return errorf(CodeUnknown, "read message: %w", err)
	}
	// Check if read more than max bytes, try to determine the size of the message.
	if r.maxBytes > 0 && len(body) > r.maxBytes {
		bytesRead := int64(len(body))
		discardedBytes, err := io.Copy(io.Discard, r.ReadCloser)
		if err != nil {
			return errorf(CodeResourceExhausted,
				"message is larger than configured max %d - unable to determine message size: %w",
				r.maxBytes, err)
		}
		return errorf(CodeResourceExhausted,
			"message size %d is larger than configured max %d",
			bytesRead+discardedBytes, r.maxBytes)
	}

	size := uint32(len(body))
	var head [5]byte
	if r.isCompressed {
		head[0] = 1
	}
	binary.BigEndian.PutUint32(head[1:], size)
	r.buf.Write(head[:])
	r.buf.Write(body)
	return nil
}

func (r *bufferedEnvelopeReader) Read(data []byte) (int, error) {
	if !r.hasInitialized {
		r.initializedErr = r.fillBuffer()
		r.hasInitialized = true
	}
	if r.initializedErr != nil {
		return 0, r.initializedErr
	}
	return r.buf.Read(data)
}

func translateConnectCommonToGRPC(request *http.Request) {
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
}

func translateConnectStreamingToGRPC(request *http.Request, contentType string) {
	translateConnectCommonToGRPC(request)
	header := request.Header

	grpcContentType := grpcContentTypePrefix + getTypeEncoding(contentType)
	setHeaderCanonical(header, headerContentType, grpcContentType)
	if contentEncoding := getHeaderCanonical(header, connectStreamingHeaderCompression); len(contentEncoding) > 0 {
		delHeaderCanonical(header, connectStreamingHeaderCompression)
		setHeaderCanonical(header, grpcHeaderCompression, contentEncoding)
	}
	if acceptEncoding := getHeaderCanonical(header, connectStreamingHeaderAcceptCompression); len(acceptEncoding) > 0 {
		delHeaderCanonical(header, connectStreamingHeaderAcceptCompression)
		setHeaderCanonical(header, grpcHeaderAcceptCompression, acceptEncoding)
	}
}

func translateConnectUnaryToGRPC(request *http.Request, contentType string, readMaxBytes int) {
	translateConnectCommonToGRPC(request)
	header := request.Header

	grpcContentType := grpcContentTypePrefix + strings.TrimPrefix(contentType, "application/")
	setHeaderCanonical(header, headerContentType, grpcContentType)
	if acceptEncoding := getHeaderCanonical(header, connectUnaryHeaderAcceptCompression); len(acceptEncoding) > 0 {
		delHeaderCanonical(header, connectUnaryHeaderAcceptCompression)
		setHeaderCanonical(header, grpcHeaderAcceptCompression, acceptEncoding)
	}

	// Handle GET request with query parameters
	if request.Method == http.MethodGet {
		request.Method = http.MethodPost
		query := request.URL.Query()
		request.URL.RawQuery = "" // clear query parameters

		isCompressed := false
		if contentEncoding := query.Get(connectUnaryCompressionQueryParameter); len(contentEncoding) > 0 {
			isCompressed = contentEncoding != "identity"
			delHeaderCanonical(header, connectUnaryHeaderCompression)
			setHeaderCanonical(header, grpcHeaderCompression, contentEncoding)
		}

		msg := query.Get(connectUnaryMessageQueryParameter)
		msgReader := queryValueReader(msg, query.Get(connectUnaryBase64QueryParameter) == "1")
		request.Body = &bufferedEnvelopeReader{
			ReadCloser:   io.NopCloser(msgReader),
			isCompressed: isCompressed,
			maxBytes:     readMaxBytes,
		}
	} else {
		isCompressed := false
		if contentEncoding := getHeaderCanonical(header, connectUnaryHeaderCompression); len(contentEncoding) > 0 {
			isCompressed = contentEncoding != "identity"
			delHeaderCanonical(header, connectUnaryHeaderCompression)
			setHeaderCanonical(header, grpcHeaderCompression, contentEncoding)
		}
		request.Body = &bufferedEnvelopeReader{
			ReadCloser:   request.Body,
			isCompressed: isCompressed,
			maxBytes:     readMaxBytes,
		}
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

type connectStreamingResponseWriter struct {
	http.ResponseWriter
	contentType string
	config      *grpcAdapterConfig
	header      http.Header
	wroteHeader bool
}

func newConnectStreamingResponseWriter(responseWriter http.ResponseWriter, contentType string, config *grpcAdapterConfig) *connectStreamingResponseWriter {
	return &connectStreamingResponseWriter{
		ResponseWriter: responseWriter,
		contentType:    contentType,
		config:         config,
	}
}

func (w *connectStreamingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *connectStreamingResponseWriter) writeHeader(statusCode int) {
	// Encode header response.
	header := w.ResponseWriter.Header()
	compression := getHeaderCanonical(w.header, grpcHeaderCompression)

	setHeaderCanonical(header, headerContentType, w.contentType)
	setHeaderCanonical(header, connectStreamingHeaderCompression, compression)

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
	if statusCode != http.StatusOK {
		w.ResponseWriter.WriteHeader(statusCode)
	}
	w.wroteHeader = true
}

func (w *connectStreamingResponseWriter) WriteHeader(statusCode int) {
	if !w.wroteHeader {
		w.writeHeader(statusCode)
	}
}

func (w *connectStreamingResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.writeHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *connectStreamingResponseWriter) finalizeStream() error {
	trailer, trailerErr := getGRPCTrailer(w.config.BufferPool, w.Header())
	if trailerErr != nil {
		trailerErr.meta = trailer
		return trailerErr
	}

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
func (w *connectStreamingResponseWriter) flushWithTrailers() {
	if err := w.finalizeStream(); err != nil {
		responseHeader := w.ResponseWriter.Header()
		setHeaderCanonical(responseHeader, headerContentType, "application/json")
		_ = w.config.ErrorWriter.writeConnectStreaming(w.ResponseWriter, err)
	}
	flushResponseWriter(w.ResponseWriter)
}

func (w *connectStreamingResponseWriter) Flush() {
	if !w.wroteHeader {
		w.writeHeader(http.StatusOK)
	}
	flushResponseWriter(w.ResponseWriter)
}

func (w *connectStreamingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

type connectUnaryResponseWriter struct {
	http.ResponseWriter
	contentType string
	config      *grpcAdapterConfig

	body        bytes.Buffer // buffered body for unary payloads
	header      http.Header  // buffered header for trailer capture
	wroteHeader bool         // whether header has been written
}

func newConnectUnaryResponseWriter(responseWriter http.ResponseWriter, contentType string, config *grpcAdapterConfig) *connectUnaryResponseWriter {
	return &connectUnaryResponseWriter{
		ResponseWriter: responseWriter,
		contentType:    contentType,
		config:         config,
	}
}

func (w *connectUnaryResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *connectUnaryResponseWriter) writeHeader(statusCode int) {
	// Encode header response.
	header := w.ResponseWriter.Header()
	compression := getHeaderCanonical(w.header, grpcHeaderCompression)

	setHeaderCanonical(header, headerContentType, w.contentType)
	setHeaderCanonical(header, connectUnaryHeaderCompression, compression)

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
	if statusCode != http.StatusOK {
		w.ResponseWriter.WriteHeader(statusCode)
	}
	w.wroteHeader = true
}

func (w *connectUnaryResponseWriter) WriteHeader(statusCode int) {
	if statusCode != http.StatusOK {
		if !w.wroteHeader {
			w.writeHeader(statusCode)
		}
	}
}

func (w *connectUnaryResponseWriter) Write(data []byte) (int, error) {
	// Write unary requests to buffered body to capture trailers and
	// allow recoding them as headers.
	wroteN, err := w.body.Write(data)
	total := w.body.Len()
	limit := int(w.config.WriteMaxBytes)
	if limit > 0 && total > limit {
		return 0, NewError(CodeResourceExhausted, fmt.Errorf("message size %d exceeds sendMaxBytes %d", total, limit))
	}
	return wroteN, err
}

func getGRPCTrailer(bufferPool *bufferPool, header http.Header) (http.Header, *Error) {
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

	trailerErr := grpcErrorFromTrailer(bufferPool, &protoBinaryCodec{}, trailer)
	if trailerErr != nil {
		return trailer, trailerErr
	}

	// Remove all protocol trailer keys.
	for key := range trailer {
		if isProtocolHeader(key) {
			delete(trailer, key)
		}
	}
	return trailer, nil
}

func (w *connectUnaryResponseWriter) finalizeUnary() error {
	if !w.wroteHeader {
		w.writeHeader(http.StatusOK)
	}

	trailer, err := getGRPCTrailer(w.config.BufferPool, w.header)
	if err != nil {
		return err
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

	_ = w.body.Next(5) // Discard envelope head.
	contentLength := strconv.Itoa(w.body.Len())
	setHeaderCanonical(responseHeader, headerContentLength, contentLength)

	// Write buffered unary body to stream.
	if _, err := w.body.WriteTo(w.ResponseWriter); err != nil {
		return err
	}
	return nil
}

func (w *connectUnaryResponseWriter) flushWithTrailers() {
	if err := w.finalizeUnary(); err != nil {
		responseHeader := w.ResponseWriter.Header()
		delHeaderCanonical(responseHeader, headerContentLength)
		delHeaderCanonical(responseHeader, connectUnaryHeaderCompression)
		setHeaderCanonical(responseHeader, headerContentType, connectUnaryContentTypeJSON)
		_ = w.config.ErrorWriter.writeConnectUnary(w.ResponseWriter, err)
	}
	flushResponseWriter(w.ResponseWriter)
}

func (w *connectUnaryResponseWriter) Flush() {}

func (w *connectUnaryResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
