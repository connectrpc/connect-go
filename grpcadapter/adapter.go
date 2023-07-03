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

package grpcadapter

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

	connect "github.com/bufbuild/connect-go"
	"golang.org/x/net/http/httpguts"
)

const (
	headerContentLength                     = "Content-Length"
	headerContentType                       = "Content-Type"
	headerTe                                = "Te"
	headerTrailer                           = "Trailer"
	connectUnaryHeaderCompression           = "Content-Encoding"
	connectUnaryHeaderAcceptCompression     = "Accept-Encoding"
	connectUnaryTrailerPrefix               = "Trailer-"
	connectStreamingHeaderCompression       = "Connect-Content-Encoding"
	connectStreamingHeaderAcceptCompression = "Connect-Accept-Encoding"
	connectHeaderTimeout                    = "Connect-Timeout-Ms"
	connectHeaderProtocolVersion            = "Connect-Protocol-Version"
	connectUnaryEncodingQueryParameter      = "encoding"
	connectUnaryMessageQueryParameter       = "message"
	connectUnaryBase64QueryParameter        = "base64"
	connectUnaryCompressionQueryParameter   = "compression"
	connectUnaryConnectQueryParameter       = "connect"
	headerVary                              = "Vary"
	connectUnaryContentTypePrefix           = "application/"
	connectStreamingContentTypeDefault      = "application/connect"
	connectStreamingContentTypePrefix       = "application/connect+"
	grpcHeaderCompression                   = "Grpc-Encoding"
	grpcHeaderAcceptCompression             = "Grpc-Accept-Encoding"
	grpcHeaderTimeout                       = "Grpc-Timeout"
	grpcHeaderStatus                        = "Grpc-Status"
	grpcHeaderMessage                       = "Grpc-Message"
	grpcHeaderDetails                       = "Grpc-Status-Details-Bin"
	grpcContentTypeDefault                  = "application/grpc"
	grpcWebContentTypeDefault               = "application/grpc-web"
	grpcContentTypePrefix                   = grpcContentTypeDefault + "+"
	grpcWebContentTypePrefix                = grpcWebContentTypeDefault + "+"
)

// NewHandler returns a new http.Handler that transcodes connect and gRPC-Web
// protocol requests to gRPC.
//
// The adapter supports unary and streaming requests and responses but does not
// validate the transport can support bidi streaming. Connect unary requests and
// responses are buffered in memory and subject to the configured limits.
// Other protocols and connect streaming are not buffered and defer to the
// handlers limits.
//
// To use the adapter, pass in a http.Handler that serves gRPC requests such as:
//
//	grpcServer := grpc.NewServer()
//	mux := grpcadapter.NewHandler(grpcServer,
//		grpcadapter.WithReadMaxBuffer(1024*1024),
//		grpcadapter.WithWriteMaxBuffer(1024*1024),
//	)
//	http.ListenAndServe(":8080", h2c.NewHandler(mux, &http2.Server{}))
func NewHandler(grpcHandler http.Handler, options ...Option) http.Handler {
	config := newConfig(options)

	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		method := request.Method
		header := request.Header
		contentType := getHeaderCanonical(header, headerContentType)

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
			ww := newGRPCWebResponseWriter(responseWriter, contentType, config)
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
			translateConnectUnaryToGRPC(request, contentType, config)
			ww := newConnectUnaryResponseWriter(responseWriter, contentType, config)
			grpcHandler.ServeHTTP(ww, request)
			ww.flushWithTrailers()
		default:
			// unknown -> grpc
			grpcHandler.ServeHTTP(responseWriter, request)
		}
	})
}

// An Option configures [NewHandler].
type Option interface {
	applyToConfig(*config)
}

type optionFunc func(*config)

func (f optionFunc) applyToConfig(config *config) {
	f(config)
}

// WithReadMaxBuffer returns a new Option that sets the maximum number of
// bytes that can be buffered for a connect unary request.
func WithReadMaxBuffer(readMaxBytes int) Option {
	return optionFunc(func(config *config) {
		config.ReadMaxBytes = readMaxBytes
	})
}

// WithWriteMaxBuffer returns a new Option that sets the maximum
// number of bytes that can be buffered for a connect unary response.
func WithWriteMaxBuffer(writeMaxBytes int) Option {
	return optionFunc(func(config *config) {
		config.WriteMaxBytes = writeMaxBytes
	})
}

type config struct {
	ReadMaxBytes  int
	WriteMaxBytes int
	ErrorWriter   *connect.ErrorWriter
	BufferPool    *bufferPool
}

func newConfig(options []Option) *config {
	config := &config{
		ErrorWriter: connect.NewErrorWriter(),
		BufferPool: &bufferPool{
			initialBufferSize:    512,
			maxRecycleBufferSize: 8 * 1024 * 1024,
		},
	}
	for _, option := range options {
		option.applyToConfig(config)
	}
	return config
}
func translateGRPCWebToGRPC(request *http.Request, contentType string) {
	request.ProtoMajor = 2
	request.ProtoMinor = 0
	header := request.Header
	ensureTeTrailers(header)

	delete(header, headerContentLength)
	grpcContentType := grpcContentTypePrefix + getTypeEncoding(contentType)
	header[headerContentType] = []string{grpcContentType}
}

type grpcWebResponseWriter struct {
	http.ResponseWriter

	config      *config
	contentType string
	seenHeaders map[string]struct{}
	wroteHeader bool
}

func newGRPCWebResponseWriter(responseWriter http.ResponseWriter, contentType string, config *config) *grpcWebResponseWriter {
	return &grpcWebResponseWriter{
		ResponseWriter: responseWriter,
		contentType:    contentType,
		config:         config,
	}
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

func (w *grpcWebResponseWriter) writeHeaders() {
	header := w.Header()

	isTrailer := map[string]struct{}{
		// Force GRPC status values to be trailers.
		grpcHeaderStatus:  struct{}{},
		grpcHeaderDetails: struct{}{},
		grpcHeaderMessage: struct{}{},
	}
	for _, key := range strings.Split(header.Get(headerTrailer), ",") {
		key = http.CanonicalHeaderKey(key)
		isTrailer[key] = struct{}{}
	}

	keys := make(map[string]struct{}, len(header))
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
		if _, ok := isTrailer[key]; ok {
			continue
		}
		keys[key] = struct{}{}
	}

	header[headerContentType] = []string{w.contentType}

	w.seenHeaders = keys
	w.wroteHeader = true
}

func (w *grpcWebResponseWriter) writeTrailer() error {
	header := w.Header()
	trailer := make(http.Header, len(header)-len(w.seenHeaders))
	for key, values := range header {
		if _, ok := w.seenHeaders[key]; ok {
			continue
		}
		key = strings.TrimPrefix(key, http.TrailerPrefix)
		// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-WEB.md#protocol-differences-vs-grpc-over-http2
		trailer[strings.ToLower(key)] = values
	}

	buf := w.config.BufferPool.Get()
	defer w.config.BufferPool.Put(buf)
	if err := trailer.Write(buf); err != nil {
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

	config         *config
	buf            *bytes.Buffer // lazy initialized
	hasInitialized bool
	initializedErr error
	isCompressed   bool
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
func (r *bufferedEnvelopeReader) Close() error {
	if r.buf != nil {
		r.config.BufferPool.Put(r.buf)
		r.buf = nil
	}
	if r.initializedErr != nil {
		r.initializedErr = io.EOF
	}
	return r.ReadCloser.Close()
}

func (r *bufferedEnvelopeReader) fillBuffer() error {
	r.buf = r.config.BufferPool.Get()

	var bodyReader io.Reader = r.ReadCloser
	if r.config.ReadMaxBytes > 0 {
		bodyReader = io.LimitReader(bodyReader, int64(r.config.ReadMaxBytes)+1)
	}
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		err := fmt.Errorf("read message: %w", err)
		return connect.NewError(connect.CodeUnknown, err)
	}
	// Check if read more than max bytes, try to determine the size of the message.
	if maxBytes := r.config.ReadMaxBytes; maxBytes > 0 && len(body) > maxBytes {
		bytesRead := int64(len(body))
		discardedBytes, err := io.Copy(io.Discard, r.ReadCloser)
		if err != nil {
			err := fmt.Errorf(
				"message is larger than configured max %d - unable to determine message size: %w",
				maxBytes, err)
			return connect.NewError(connect.CodeResourceExhausted, err)
		}
		err = fmt.Errorf(
			"message size %d is larger than configured max %d",
			bytesRead+discardedBytes, maxBytes)
		return connect.NewError(connect.CodeResourceExhausted, err)
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

type envelopeReader struct {
	io.ReadCloser

	head  [5]byte
	index int
}

func (r *envelopeReader) Read(data []byte) (int, error) {
	if r.index < len(r.head) {
		n := copy(data, r.head[r.index:])
		r.index += n
		return n, nil
	}
	return r.ReadCloser.Read(data)
}

func translateConnectCommonToGRPC(request *http.Request) {
	request.ProtoMajor = 2
	request.ProtoMinor = 0
	header := request.Header
	ensureTeTrailers(header)

	delete(header, connectHeaderProtocolVersion)
	delete(header, headerContentLength)

	// Translate timeout header
	if timeout := getHeaderCanonical(header, connectHeaderTimeout); len(timeout) > 0 {
		delete(header, connectHeaderTimeout)
		header[grpcHeaderTimeout] = []string{timeout + "m"}
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

func translateConnectUnaryToGRPC(request *http.Request, contentType string, config *config) {
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
			config:       config,
			isCompressed: isCompressed,
		}
		return
	}
	isCompressed := false
	if contentEncoding := getHeaderCanonical(header, connectUnaryHeaderCompression); len(contentEncoding) > 0 {
		isCompressed = contentEncoding != "identity"
		delHeaderCanonical(header, connectUnaryHeaderCompression)
		setHeaderCanonical(header, grpcHeaderCompression, contentEncoding)
	}
	if request.ContentLength <= 0 {
		// no content length, buffer message
		request.Body = &bufferedEnvelopeReader{
			ReadCloser:   request.Body,
			config:       config,
			isCompressed: isCompressed,
		}
		return
	}
	var head [5]byte
	if isCompressed {
		head[0] = 1
	}
	size := uint32(request.ContentLength) // > 0
	binary.BigEndian.PutUint32(head[1:], size)
	request.Body = &envelopeReader{
		ReadCloser: request.Body,
		head:       head,
	}
}

type connectStreamingResponseWriter struct {
	http.ResponseWriter

	contentType string
	config      *config
	header      http.Header
	wroteHeader bool
}

func newConnectStreamingResponseWriter(responseWriter http.ResponseWriter, contentType string, config *config) *connectStreamingResponseWriter {
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

func (w *connectStreamingResponseWriter) Flush() {
	if !w.wroteHeader {
		w.writeHeader(http.StatusOK)
	}
	flushResponseWriter(w.ResponseWriter)
}

func (w *connectStreamingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
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
	w.ResponseWriter.WriteHeader(statusCode)
	w.wroteHeader = true
}

func (w *connectStreamingResponseWriter) finalizeStream() *connect.Error {
	trailer, trailerErr := getGRPCTrailer(w.Header())
	if trailerErr != nil {
		meta := trailerErr.Meta()
		for key, vals := range trailer {
			meta[key] = append(meta[key], vals...)
		}
		return trailerErr
	}

	// Encode as connect end message.
	end := &struct {
		Metadata http.Header `json:"metadata,omitempty"`
	}{
		Metadata: trailer,
	}
	data, err := json.Marshal(end)
	if err != nil {
		err := fmt.Errorf("failed to marshal connect end message: %w", err)
		return connect.NewError(connect.CodeInternal, err)
	}

	head := [5]byte{1 << 1, 0, 0, 0, 0} // 1 << 1 = 2 = end stream
	binary.BigEndian.PutUint32(head[1:5], uint32(len(data)))
	if _, err := w.ResponseWriter.Write(head[:]); err != nil {
		err := fmt.Errorf("failed to write head: %w", err)
		return connect.NewError(connect.CodeInternal, err)
	}
	if _, err := w.ResponseWriter.Write(data); err != nil {
		err := fmt.Errorf("failed to write end response: %w", err)
		return connect.NewError(connect.CodeInternal, err)
	}
	return nil
}
func (w *connectStreamingResponseWriter) flushWithTrailers() {
	if err := w.finalizeStream(); err != nil {
		responseHeader := w.ResponseWriter.Header()
		setHeaderCanonical(responseHeader, headerContentType, "application/json")
		// Stub request to satisfy error writer.
		request := &http.Request{Header: map[string][]string{"Content-Type": {w.contentType}}}
		_ = w.config.ErrorWriter.Write(w.ResponseWriter, request, err)
	}
	flushResponseWriter(w.ResponseWriter)
}

type connectUnaryResponseWriter struct {
	http.ResponseWriter

	contentType string
	config      *config
	body        *bytes.Buffer // buffered body for unary payloads
	header      http.Header   // buffered header for trailer capture
	wroteHeader bool          // whether header has been written
}

func newConnectUnaryResponseWriter(responseWriter http.ResponseWriter, contentType string, config *config) *connectUnaryResponseWriter {
	return &connectUnaryResponseWriter{
		ResponseWriter: responseWriter,
		contentType:    contentType,
		config:         config,
		body:           config.BufferPool.Get(),
	}
}

func (w *connectUnaryResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
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
	limit := w.config.WriteMaxBytes
	if limit > 0 && total > limit {
		err := fmt.Errorf("message size %d exceeds sendMaxBytes %d", total, limit)
		return 0, connect.NewError(connect.CodeResourceExhausted, err)
	}
	return wroteN, err
}

func (w *connectUnaryResponseWriter) Flush() {}

func (w *connectUnaryResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
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
	// Don't send headers to caputre trailers, unless there is an error.
	if statusCode != http.StatusOK {
		w.ResponseWriter.WriteHeader(statusCode)
	}
	w.wroteHeader = true
}

func (w *connectUnaryResponseWriter) finalizeUnary() *connect.Error {
	defer func() {
		w.config.BufferPool.Put(w.body)
		w.body = nil
	}()
	if !w.wroteHeader {
		w.writeHeader(http.StatusOK)
	}

	trailer, trailerErr := getGRPCTrailer(w.header)
	if trailerErr != nil {
		return trailerErr
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
		err := fmt.Errorf("failed to write buffered body: %w", err)
		return connect.NewError(connect.CodeInternal, err)
	}
	return nil
}

func (w *connectUnaryResponseWriter) flushWithTrailers() {
	if err := w.finalizeUnary(); err != nil {
		responseHeader := w.ResponseWriter.Header()
		delHeaderCanonical(responseHeader, headerContentLength)
		delHeaderCanonical(responseHeader, connectUnaryHeaderCompression)
		setHeaderCanonical(responseHeader, headerContentType, "application/json")
		// Stub request to satisfy error writer.
		request := &http.Request{Header: map[string][]string{"Content-Type": {w.contentType}}}
		_ = w.config.ErrorWriter.Write(w.ResponseWriter, request, err)
	}
	flushResponseWriter(w.ResponseWriter)
}

func flushResponseWriter(responseWriter http.ResponseWriter) {
	responseWriter.(http.Flusher).Flush()
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
	teValues := header[headerTe]
	if !httpguts.HeaderValuesContainsToken(teValues, "trailers") {
		header[headerTe] = append(teValues, "trailers")
	}
}

func getTypeEncoding(contentType string) string {
	if i := strings.LastIndex(contentType, "+"); i >= 0 {
		return contentType[i+1:]
	}
	return "proto"
}
