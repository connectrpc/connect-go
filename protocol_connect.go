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
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/anypb"
)

const (
	connectUnaryHeaderCompression           = "Content-Encoding"
	connectUnaryHeaderAcceptCompression     = "Accept-Encoding"
	connectUnaryTrailerPrefix               = "Trailer-"
	connectStreamingHeaderCompression       = "Connect-Content-Encoding"
	connectStreamingHeaderAcceptCompression = "Connect-Accept-Encoding"
	connectHeaderTimeout                    = "Connect-Timeout-Ms"
	connectHeaderProtocolVersion            = "Connect-Protocol-Version"
	connectProtocolVersion                  = "1"
	headerVary                              = "Vary"

	connectFlagEnvelopeEndStream = 0b00000010

	connectUnaryContentTypePrefix     = "application/"
	connectUnaryContentTypeJSON       = connectUnaryContentTypePrefix + "json"
	connectStreamingContentTypePrefix = "application/connect+"

	connectUnaryEncodingQueryParameter    = "encoding"
	connectUnaryMessageQueryParameter     = "message"
	connectUnaryBase64QueryParameter      = "base64"
	connectUnaryCompressionQueryParameter = "compression"
	connectUnaryConnectQueryParameter     = "connect"
	connectUnaryConnectQueryValue         = "v" + connectProtocolVersion
)

// defaultConnectUserAgent returns a User-Agent string similar to those used in gRPC.
//
//nolint:gochecknoglobals
var defaultConnectUserAgent = fmt.Sprintf("connect-go/%s (%s)", Version, runtime.Version())

type protocolConnect struct{}

// NewHandler implements protocol, so it must return an interface.
func (*protocolConnect) NewHandler(params *protocolHandlerParams) protocolHandler {
	methods := make(map[string]struct{})
	methods[http.MethodPost] = struct{}{}

	if params.Spec.StreamType == StreamTypeUnary && params.IdempotencyLevel == IdempotencyNoSideEffects {
		methods[http.MethodGet] = struct{}{}
	}

	contentTypes := make(map[string]struct{})
	for _, name := range params.Codecs.Names() {
		if params.Spec.StreamType == StreamTypeUnary {
			contentTypes[canonicalizeContentType(connectUnaryContentTypePrefix+name)] = struct{}{}
			continue
		}
		contentTypes[canonicalizeContentType(connectStreamingContentTypePrefix+name)] = struct{}{}
	}

	return &connectHandler{
		protocolHandlerParams: *params,
		methods:               methods,
		accept:                contentTypes,
	}
}

// NewClient implements protocol, so it must return an interface.
func (*protocolConnect) NewClient(params *protocolClientParams) (protocolClient, error) {
	return &connectClient{
		protocolClientParams: *params,
		peer:                 newPeerFromURL(params.URL, ProtocolConnect),
	}, nil
}

type connectHandler struct {
	protocolHandlerParams

	methods map[string]struct{}
	accept  map[string]struct{}
}

func (h *connectHandler) Methods() map[string]struct{} {
	return h.methods
}

func (h *connectHandler) ContentTypes() map[string]struct{} {
	return h.accept
}

func (*connectHandler) SetTimeout(request *http.Request) (context.Context, context.CancelFunc, error) {
	timeout := getHeaderCanonical(request.Header, connectHeaderTimeout)
	if timeout == "" {
		return request.Context(), nil, nil
	}
	if len(timeout) > 10 {
		return nil, nil, errorf(CodeInvalidArgument, "parse timeout: %q has >10 digits", timeout)
	}
	millis, err := strconv.ParseInt(timeout, 10 /* base */, 64 /* bitsize */)
	if err != nil {
		return nil, nil, errorf(CodeInvalidArgument, "parse timeout: %w", err)
	}
	ctx, cancel := context.WithTimeout(
		request.Context(),
		time.Duration(millis)*time.Millisecond,
	)
	return ctx, cancel, nil
}

func (h *connectHandler) CanHandlePayload(request *http.Request, contentType string) bool {
	if request.Method == http.MethodGet {
		query := request.URL.Query()
		codecName := query.Get(connectUnaryEncodingQueryParameter)
		contentType = connectContentTypeFromCodecName(
			h.Spec.StreamType,
			codecName,
		)
	}
	_, ok := h.accept[contentType]
	return ok
}

func (h *connectHandler) NewConn(
	responseWriter http.ResponseWriter,
	request *http.Request,
) (handlerConnCloser, bool) {
	query := request.URL.Query()
	// We need to parse metadata before entering the interceptor stack; we'll
	// send the error to the client later on.
	var contentEncoding, acceptEncoding string
	if h.Spec.StreamType == StreamTypeUnary {
		if request.Method == http.MethodGet {
			contentEncoding = query.Get(connectUnaryCompressionQueryParameter)
		} else {
			contentEncoding = getHeaderCanonical(request.Header, connectUnaryHeaderCompression)
		}
		acceptEncoding = getHeaderCanonical(request.Header, connectUnaryHeaderAcceptCompression)
	} else {
		contentEncoding = getHeaderCanonical(request.Header, connectStreamingHeaderCompression)
		acceptEncoding = getHeaderCanonical(request.Header, connectStreamingHeaderAcceptCompression)
	}
	requestCompression, responseCompression, failed := negotiateCompression(
		h.CompressionPools,
		contentEncoding,
		acceptEncoding,
	)
	if failed == nil {
		failed = checkServerStreamsCanFlush(h.Spec, responseWriter)
	}
	if failed == nil && request.Method == http.MethodGet {
		version := query.Get(connectUnaryConnectQueryParameter)
		if version == "" && h.RequireConnectProtocolHeader {
			failed = errorf(CodeInvalidArgument, "missing required query parameter: set %s to %q", connectUnaryConnectQueryParameter, connectUnaryConnectQueryValue)
		} else if version != "" && version != connectUnaryConnectQueryValue {
			failed = errorf(CodeInvalidArgument, "%s must be %q: got %q", connectUnaryConnectQueryParameter, connectUnaryConnectQueryValue, version)
		}
	}
	if failed == nil && request.Method == http.MethodPost {
		version := getHeaderCanonical(request.Header, connectHeaderProtocolVersion)
		if version == "" && h.RequireConnectProtocolHeader {
			failed = errorf(CodeInvalidArgument, "missing required header: set %s to %q", connectHeaderProtocolVersion, connectProtocolVersion)
		} else if version != "" && version != connectProtocolVersion {
			failed = errorf(CodeInvalidArgument, "%s must be %q: got %q", connectHeaderProtocolVersion, connectProtocolVersion, version)
		}
	}

	var requestBody io.ReadCloser
	var contentType, codecName string
	if request.Method == http.MethodGet {
		if failed == nil && !query.Has(connectUnaryEncodingQueryParameter) {
			failed = errorf(CodeInvalidArgument, "missing %s parameter", connectUnaryEncodingQueryParameter)
		} else if failed == nil && !query.Has(connectUnaryMessageQueryParameter) {
			failed = errorf(CodeInvalidArgument, "missing %s parameter", connectUnaryMessageQueryParameter)
		}
		msg := query.Get(connectUnaryMessageQueryParameter)
		msgReader := queryValueReader(msg, query.Get(connectUnaryBase64QueryParameter) == "1")
		requestBody = io.NopCloser(msgReader)
		codecName = query.Get(connectUnaryEncodingQueryParameter)
		contentType = connectContentTypeFromCodecName(
			h.Spec.StreamType,
			codecName,
		)
	} else {
		requestBody = request.Body
		contentType = getHeaderCanonical(request.Header, headerContentType)
		codecName = connectCodecFromContentType(
			h.Spec.StreamType,
			contentType,
		)
	}

	codec := h.Codecs.Get(codecName)
	// The codec can be nil in the GET request case; that's okay: when failed
	// is non-nil, codec is never used.
	if failed == nil && codec == nil {
		failed = errorf(CodeInvalidArgument, "invalid message encoding: %q", codecName)
	}

	// Write any remaining headers here:
	// (1) any writes to the stream will implicitly send the headers, so we
	// should get all of gRPC's required response headers ready.
	// (2) interceptors should be able to see these headers.
	//
	// Since we know that these header keys are already in canonical form, we can
	// skip the normalization in Header.Set.
	header := responseWriter.Header()
	header[headerContentType] = []string{contentType}
	acceptCompressionHeader := connectUnaryHeaderAcceptCompression
	if h.Spec.StreamType != StreamTypeUnary {
		acceptCompressionHeader = connectStreamingHeaderAcceptCompression
		// We only write the request encoding header here for streaming calls,
		// since the streaming envelope lets us choose whether to compress each
		// message individually. For unary, we won't know whether we're compressing
		// the request until we see how large the payload is.
		if responseCompression != compressionIdentity {
			header[connectStreamingHeaderCompression] = []string{responseCompression}
		}
	}
	header[acceptCompressionHeader] = []string{h.CompressionPools.CommaSeparatedNames()}

	var conn handlerConnCloser
	peer := Peer{
		Addr:     request.RemoteAddr,
		Protocol: ProtocolConnect,
		Query:    query,
	}
	if h.Spec.StreamType == StreamTypeUnary {
		conn = &connectUnaryHandlerConn{
			spec:                    h.Spec,
			peer:                    peer,
			request:                 request,
			requestBody:             requestBody,
			responseWriter:          responseWriter,
			responseTrailer:         make(http.Header),
			codec:                   codec,
			compressMinBytes:        h.CompressMinBytes,
			compressionRequestName:  requestCompression,
			compressionRequestPool:  h.CompressionPools.Get(requestCompression),
			compressionResponseName: responseCompression,
			compressionResponsePool: h.CompressionPools.Get(responseCompression),
			bufferPool:              h.BufferPool,
			sendMaxBytes:            h.SendMaxBytes,
			readMaxBytes:            h.ReadMaxBytes,
		}
	} else {
		conn = &connectStreamingHandlerConn{
			spec:                    h.Spec,
			peer:                    peer,
			request:                 request,
			requestBody:             requestBody,
			responseWriter:          responseWriter,
			responseTrailer:         make(http.Header),
			codec:                   codec,
			compressionRequestPool:  h.CompressionPools.Get(requestCompression),
			compressionResponsePool: h.CompressionPools.Get(responseCompression),
			bufferPool:              h.BufferPool,
			compressMinBytes:        h.CompressMinBytes,
			sendMaxBytes:            h.SendMaxBytes,
			readMaxBytes:            h.ReadMaxBytes,
		}
	}
	conn = wrapHandlerConnWithCodedErrors(conn)

	if failed != nil {
		// Negotiation failed, so we can't establish a stream.
		_ = conn.Close(failed)
		return nil, false
	}
	return conn, true
}

type connectClient struct {
	protocolClientParams

	peer Peer
}

func (c *connectClient) Peer() Peer {
	return c.peer
}

func (c *connectClient) WriteRequestHeader(streamType StreamType, header http.Header) {
	// We know these header keys are in canonical form, so we can bypass all the
	// checks in Header.Set.
	if getHeaderCanonical(header, headerUserAgent) == "" {
		header[headerUserAgent] = []string{defaultConnectUserAgent}
	}
	header[connectHeaderProtocolVersion] = []string{connectProtocolVersion}
	header[headerContentType] = []string{
		connectContentTypeFromCodecName(streamType, c.Codec.Name()),
	}
	acceptCompressionHeader := connectUnaryHeaderAcceptCompression
	if streamType != StreamTypeUnary {
		// If we don't set Accept-Encoding, by default http.Client will ask the
		// server to compress the whole stream. Since we're already compressing
		// each message, this is a waste.
		header[connectUnaryHeaderAcceptCompression] = []string{compressionIdentity}
		acceptCompressionHeader = connectStreamingHeaderAcceptCompression
		// We only write the request encoding header here for streaming calls,
		// since the streaming envelope lets us choose whether to compress each
		// message individually. For unary, we won't know whether we're compressing
		// the request until we see how large the payload is.
		if c.CompressionName != "" && c.CompressionName != compressionIdentity {
			header[connectStreamingHeaderCompression] = []string{c.CompressionName}
		}
	}
	if acceptCompression := c.CompressionPools.CommaSeparatedNames(); acceptCompression != "" {
		header[acceptCompressionHeader] = []string{acceptCompression}
	}
}

func (c *connectClient) NewConn(
	ctx context.Context,
	spec Spec,
	header http.Header,
) streamingClientConn {
	if deadline, ok := ctx.Deadline(); ok {
		millis := int64(time.Until(deadline) / time.Millisecond)
		if millis > 0 {
			encoded := strconv.FormatInt(millis, 10 /* base */)
			if len(encoded) <= 10 {
				header[connectHeaderTimeout] = []string{encoded}
			} // else effectively unbounded
		}
	}
	duplexCall := newDuplexHTTPCall(ctx, c.HTTPClient, c.URL, spec, header)
	var conn streamingClientConn
	if spec.StreamType == StreamTypeUnary {
		unaryConn := &connectUnaryClientConn{
			spec:             spec,
			peer:             c.Peer(),
			duplexCall:       duplexCall,
			compressionPools: c.CompressionPools,
			bufferPool:       c.BufferPool,
			responseHeader:   make(http.Header),
			responseTrailer:  make(http.Header),
			compressMinBytes: c.CompressMinBytes,
			compressionName:  c.CompressionName,
			compressionPool:  c.CompressionPools.Get(c.CompressionName),
			codec:            c.Codec,
			readMaxBytes:     c.ReadMaxBytes,
			sendMaxBytes:     c.SendMaxBytes,
		}
		if spec.IdempotencyLevel == IdempotencyNoSideEffects {
			unaryConn.enableGet = c.EnableGet
			unaryConn.getURLMaxBytes = c.GetURLMaxBytes
			unaryConn.getUseFallback = c.GetUseFallback
			unaryConn.duplexCall = duplexCall
			if stableCodec, ok := c.Codec.(stableCodec); ok {
				unaryConn.stableCodec = stableCodec
			}
		}
		conn = unaryConn
		duplexCall.SetValidateResponse(unaryConn.validateResponse)
	} else {
		streamingConn := &connectStreamingClientConn{
			spec:                   spec,
			peer:                   c.Peer(),
			duplexCall:             duplexCall,
			compressionPools:       c.CompressionPools,
			compressionRequestPool: c.CompressionPools.Get(c.CompressionName),
			bufferPool:             c.BufferPool,
			codec:                  c.Codec,
			responseHeader:         make(http.Header),
			responseTrailer:        make(http.Header),
			compressMinBytes:       c.CompressMinBytes,
			sendMaxBytes:           c.SendMaxBytes,
			readMaxBytes:           c.ReadMaxBytes,
		}
		conn = streamingConn
		duplexCall.SetValidateResponse(streamingConn.validateResponse)
	}
	return wrapClientConnWithCodedErrors(conn)
}

type connectUnaryClientConn struct {
	spec             Spec
	peer             Peer
	duplexCall       *duplexHTTPCall
	compressionPools readOnlyCompressionPools
	compressionPool  *compressionPool // set by validateResponse
	bufferPool       *bufferPool
	responseHeader   http.Header
	responseTrailer  http.Header

	codec            Codec
	compressMinBytes int
	compressionName  string
	readMaxBytes     int
	sendMaxBytes     int
	alreadyRead      bool

	// Get-related fields
	enableGet      bool
	getURLMaxBytes int
	getUseFallback bool
	stableCodec    stableCodec
}

func (cc *connectUnaryClientConn) Spec() Spec {
	return cc.spec
}

func (cc *connectUnaryClientConn) Peer() Peer {
	return cc.peer
}

func (cc *connectUnaryClientConn) Send(msg any) error {
	buffer := cc.bufferPool.Get()
	defer cc.bufferPool.Put(buffer)

	if cc.enableGet {
		if err := cc.trySendGet(buffer, msg); err != nil {
			return err
		}
		return nil
	}
	if err := cc.sendMsg(buffer, msg); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *Error is a non-nil error
}

func (cc *connectUnaryClientConn) RequestHeader() http.Header {
	return cc.duplexCall.Header()
}

func (cc *connectUnaryClientConn) CloseRequest() error {
	return cc.duplexCall.CloseWrite()
}

func (cc *connectUnaryClientConn) Receive(msg any) error {
	if cc.alreadyRead {
		return io.EOF
	}
	cc.alreadyRead = true
	cc.duplexCall.BlockUntilResponseReady()
	buffer := cc.bufferPool.Get()
	defer cc.bufferPool.Put(buffer)
	if err := readAll(buffer, cc.duplexCall, cc.readMaxBytes); err != nil {
		return err
	}
	if cc.compressionPool != nil {
		if err := decompress(buffer, cc.bufferPool, cc.compressionPool, cc.readMaxBytes); err != nil {
			return err
		}
	}
	if err := unmarshal(buffer, msg, cc.codec); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *Error is a non-nil error
}

func (cc *connectUnaryClientConn) ResponseHeader() http.Header {
	cc.duplexCall.BlockUntilResponseReady()
	return cc.responseHeader
}

func (cc *connectUnaryClientConn) ResponseTrailer() http.Header {
	cc.duplexCall.BlockUntilResponseReady()
	return cc.responseTrailer
}

func (cc *connectUnaryClientConn) CloseResponse() error {
	return cc.duplexCall.CloseRead()
}

func (cc *connectUnaryClientConn) onRequestSend(fn func(*http.Request)) {
	cc.duplexCall.onRequestSend = fn
}

func (cc *connectUnaryClientConn) validateResponse(response *http.Response) *Error {
	for k, v := range response.Header {
		if !strings.HasPrefix(k, connectUnaryTrailerPrefix) {
			cc.responseHeader[k] = v
			continue
		}
		cc.responseTrailer[strings.TrimPrefix(k, connectUnaryTrailerPrefix)] = v
	}
	compression := getHeaderCanonical(response.Header, connectUnaryHeaderCompression)
	if compression != "" &&
		compression != compressionIdentity &&
		!cc.compressionPools.Contains(compression) {
		return errorf(
			CodeInternal,
			"unknown encoding %q: accepted encodings are %v",
			compression,
			cc.compressionPools.CommaSeparatedNames(),
		)
	}

	cc.compressionPool = cc.compressionPools.Get(compression)
	if response.StatusCode == http.StatusOK {
		return nil
	}
	if response.StatusCode == http.StatusNotModified && cc.Spec().IdempotencyLevel == IdempotencyNoSideEffects {
		serverErr := NewWireError(CodeUnknown, errNotModifiedClient)
		// RFC 9110 doesn't allow trailers on 304s, so we only need to include headers.
		serverErr.meta = cc.responseHeader.Clone()
		return serverErr
	}
	data := cc.bufferPool.Get()
	defer cc.bufferPool.Put(data)

	if err := readAll(data, response.Body, cc.readMaxBytes); err != nil {
		return err
	}
	if cc.compressionPool != nil {
		if err := decompress(data, cc.bufferPool, cc.compressionPool, cc.readMaxBytes); err != nil {
			return err
		}
	}
	var wireErr connectWireError
	if err := json.Unmarshal(data.Bytes(), &wireErr); err != nil {
		return errorf(CodeInternal, "failed to unmarshal error: %v", err)
	}

	serverErr := wireErr.asError()
	if serverErr == nil {
		return nil
	}
	serverErr.meta = cc.responseHeader.Clone()
	mergeHeaders(serverErr.meta, cc.responseTrailer)
	return serverErr
}

type connectStreamingClientConn struct {
	spec                    Spec
	peer                    Peer
	duplexCall              *duplexHTTPCall
	compressionPools        readOnlyCompressionPools
	codec                   Codec
	responseHeader          http.Header
	responseTrailer         http.Header
	compressionRequestPool  *compressionPool
	compressionResponsePool *compressionPool
	bufferPool              *bufferPool
	compressMinBytes        int
	sendMaxBytes            int
	readMaxBytes            int
}

func (cc *connectStreamingClientConn) Spec() Spec {
	return cc.spec
}

func (cc *connectStreamingClientConn) Peer() Peer {
	return cc.peer
}

func (cc *connectStreamingClientConn) Send(msg any) error {
	buffer := cc.bufferPool.Get()
	defer cc.bufferPool.Put(buffer)

	if err := marshal(buffer, msg, cc.codec); err != nil {
		return err
	}
	var flags uint8
	if cc.compressionRequestPool != nil && buffer.Len() > cc.compressMinBytes {
		if err := compress(buffer, cc.bufferPool, cc.compressionRequestPool); err != nil {
			return err
		}
		flags |= flagEnvelopeCompressed
	}
	if err := checkSendMaxBytes(buffer, cc.sendMaxBytes, flags&flagEnvelopeCompressed > 0); err != nil {
		return err
	}
	if err := writeEnvelope(cc.duplexCall, flags, buffer); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *error is a non-nil error
}

func (cc *connectStreamingClientConn) RequestHeader() http.Header {
	return cc.duplexCall.Header()
}

func (cc *connectStreamingClientConn) CloseRequest() error {
	return cc.duplexCall.CloseWrite()
}

func (cc *connectStreamingClientConn) Receive(msg any) error {
	cc.duplexCall.BlockUntilResponseReady()
	buffer := cc.bufferPool.Get()
	defer cc.bufferPool.Put(buffer)

	flags, err := readEnvelope(buffer, cc.duplexCall, cc.readMaxBytes)
	if err != nil {
		// If the error is EOF but not from a last message, we want to return
		// io.ErrUnexpectedEOF instead.
		if errors.Is(err, io.EOF) {
			err = errorf(CodeInternal, "protocol error: %w", io.ErrUnexpectedEOF)
		}
		return err
	}
	if flags&flagEnvelopeCompressed != 0 {
		if cc.compressionResponsePool == nil {
			return errorf(CodeInvalidArgument,
				"protocol error: received compressed message without %s header",
				connectStreamingHeaderCompression,
			)
		}
		if err := decompress(buffer, cc.bufferPool, cc.compressionResponsePool, cc.readMaxBytes); err != nil {
			return err
		}
	}
	if flags != 0 && flags != flagEnvelopeCompressed {
		end, err := connectUnmarshalEndStreamMessage(buffer, flags)
		if err != nil {
			return err
		}
		// See if the server sent an explicit error in the end-of-stream message.
		mergeHeaders(cc.responseTrailer, end.Trailer)
		if serverErr := end.Error.asError(); serverErr != nil {
			// This is expected from a protocol perspective, but receiving an
			// end-of-stream message means that we're _not_ getting a regular message.
			// For users to realize that the stream has ended, Receive must return an
			// error.
			serverErr.meta = cc.responseHeader.Clone()
			mergeHeaders(serverErr.meta, cc.responseTrailer)
			cc.duplexCall.SetError(serverErr)
			return serverErr
		}
		return io.EOF
	}

	if err := unmarshal(buffer, msg, cc.codec); err != nil {
		// There's no error in the trailers, so this was probably an error
		// converting the bytes to a message, an error reading from the network, or
		// just an EOF. We're going to return it to the user, but we also want to
		// setResponseError so Send errors out.
		cc.duplexCall.SetError(err)
		return err
	}
	return nil // must be a literal nil: nil *Error is a non-nil error
}

func (cc *connectStreamingClientConn) ResponseHeader() http.Header {
	cc.duplexCall.BlockUntilResponseReady()
	return cc.responseHeader
}

func (cc *connectStreamingClientConn) ResponseTrailer() http.Header {
	cc.duplexCall.BlockUntilResponseReady()
	return cc.responseTrailer
}

func (cc *connectStreamingClientConn) CloseResponse() error {
	return cc.duplexCall.CloseRead()
}

func (cc *connectStreamingClientConn) onRequestSend(fn func(*http.Request)) {
	cc.duplexCall.onRequestSend = fn
}

func (cc *connectStreamingClientConn) validateResponse(response *http.Response) *Error {
	if response.StatusCode != http.StatusOK {
		return errorf(connectHTTPToCode(response.StatusCode), "HTTP status %v", response.Status)
	}
	compression := getHeaderCanonical(response.Header, connectStreamingHeaderCompression)
	if compression != "" &&
		compression != compressionIdentity &&
		!cc.compressionPools.Contains(compression) {
		return errorf(
			CodeInternal,
			"unknown encoding %q: accepted encodings are %v",
			compression,
			cc.compressionPools.CommaSeparatedNames(),
		)
	}
	cc.compressionResponsePool = cc.compressionPools.Get(compression)
	mergeHeaders(cc.responseHeader, response.Header)
	return nil
}

type connectUnaryHandlerConn struct {
	spec                    Spec
	peer                    Peer
	request                 *http.Request
	requestBody             io.ReadCloser
	responseWriter          http.ResponseWriter
	responseTrailer         http.Header
	codec                   Codec
	compressMinBytes        int
	compressionRequestName  string
	compressionRequestPool  *compressionPool
	compressionResponseName string
	compressionResponsePool *compressionPool
	bufferPool              *bufferPool
	sendMaxBytes            int
	readMaxBytes            int
	alreadyRead             bool
	wroteBody               bool
}

func (hc *connectUnaryHandlerConn) Spec() Spec {
	return hc.spec
}

func (hc *connectUnaryHandlerConn) Peer() Peer {
	return hc.peer
}

func (hc *connectUnaryHandlerConn) Receive(msg any) error {
	if hc.alreadyRead {
		return NewError(CodeInternal, io.EOF)
	}
	hc.alreadyRead = true

	buffer := hc.bufferPool.Get()
	defer hc.bufferPool.Put(buffer)

	if err := readAll(buffer, hc.requestBody, hc.readMaxBytes); err != nil {
		return err
	}
	if buffer.Len() > 0 && hc.compressionRequestPool != nil {
		if err := decompress(buffer, hc.bufferPool, hc.compressionRequestPool, hc.readMaxBytes); err != nil {
			return err
		}
	}
	if err := unmarshal(buffer, msg, hc.codec); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *Error is a non-nil error
}

func (hc *connectUnaryHandlerConn) RequestHeader() http.Header {
	return hc.request.Header
}

func (hc *connectUnaryHandlerConn) Send(msg any) error {
	buffer := hc.bufferPool.Get()
	defer hc.bufferPool.Put(buffer)

	hc.wroteBody = true
	hc.writeResponseHeader(nil /* error */)
	header := hc.responseWriter.Header()

	if err := marshal(buffer, msg, hc.codec); err != nil {
		return err
	}
	var isCompressed bool
	if buffer.Len() > hc.compressMinBytes && hc.compressionResponsePool != nil {
		if err := compress(buffer, hc.bufferPool, hc.compressionResponsePool); err != nil {
			return err
		}
		setHeaderCanonical(header, connectUnaryHeaderCompression, hc.compressionResponseName)
		isCompressed = true
	}
	if err := checkSendMaxBytes(buffer, hc.sendMaxBytes, isCompressed); err != nil {
		delHeaderCanonical(header, connectUnaryHeaderCompression)
		return err
	}
	if err := writeAll(hc.responseWriter, buffer); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *Error is a non-nil error
}

func (hc *connectUnaryHandlerConn) ResponseHeader() http.Header {
	return hc.responseWriter.Header()
}

func (hc *connectUnaryHandlerConn) ResponseTrailer() http.Header {
	return hc.responseTrailer
}

func (hc *connectUnaryHandlerConn) Close(err error) error {
	if !hc.wroteBody {
		hc.writeResponseHeader(err)
		// If the handler received a GET request and the resource hasn't changed,
		// return a 304.
		if len(hc.peer.Query) > 0 && IsNotModifiedError(err) {
			hc.responseWriter.WriteHeader(http.StatusNotModified)
			return hc.request.Body.Close()
		}
	}
	if err == nil {
		return hc.request.Body.Close()
	}
	// In unary Connect, errors always use application/json.
	setHeaderCanonical(hc.responseWriter.Header(), headerContentType, connectUnaryContentTypeJSON)
	hc.responseWriter.WriteHeader(connectCodeToHTTP(CodeOf(err)))
	data, marshalErr := json.Marshal(newConnectWireError(err))
	if marshalErr != nil {
		_ = hc.request.Body.Close()
		return errorf(CodeInternal, "marshal error: %w", err)
	}
	if _, writeErr := hc.responseWriter.Write(data); writeErr != nil {
		_ = hc.request.Body.Close()
		return writeErr
	}
	return hc.request.Body.Close()
}

func (hc *connectUnaryHandlerConn) getHTTPMethod() string {
	return hc.request.Method
}

func (hc *connectUnaryHandlerConn) writeResponseHeader(err error) {
	header := hc.responseWriter.Header()
	if hc.request.Method == http.MethodGet {
		// The response content varies depending on the compression that the client
		// requested (if any). GETs are potentially cacheable, so we should ensure
		// that the Vary header includes at least Accept-Encoding (and not overwrite any values already set).
		header[headerVary] = append(header[headerVary], connectUnaryHeaderAcceptCompression)
	}
	if err != nil {
		if connectErr, ok := asError(err); ok {
			mergeHeaders(header, connectErr.meta)
		}
	}
	for k, v := range hc.responseTrailer {
		header[connectUnaryTrailerPrefix+k] = v
	}
}

type connectStreamingHandlerConn struct {
	spec                    Spec
	peer                    Peer
	request                 *http.Request
	requestBody             io.ReadCloser
	responseWriter          http.ResponseWriter
	responseTrailer         http.Header
	codec                   Codec
	compressionRequestPool  *compressionPool
	compressionResponsePool *compressionPool
	bufferPool              *bufferPool
	compressMinBytes        int
	sendMaxBytes            int
	readMaxBytes            int
	end                     *connectEndStreamMessage
}

func (hc *connectStreamingHandlerConn) Spec() Spec {
	return hc.spec
}

func (hc *connectStreamingHandlerConn) Peer() Peer {
	return hc.peer
}

func (hc *connectStreamingHandlerConn) Receive(msg any) error {
	buffer := hc.bufferPool.Get()
	defer hc.bufferPool.Put(buffer)

	flags, err := readEnvelope(buffer, hc.request.Body, hc.readMaxBytes)
	if err != nil {
		return err
	}
	if flags&flagEnvelopeCompressed != 0 {
		if hc.compressionRequestPool == nil {
			return errorf(CodeInvalidArgument,
				"protocol error: received compressed message without %s header",
				connectStreamingHeaderCompression,
			)
		}
		if err := decompress(buffer, hc.bufferPool, hc.compressionRequestPool, hc.readMaxBytes); err != nil {
			return err
		}
	}
	if flags != 0 && flags != flagEnvelopeCompressed {
		end, err := connectUnmarshalEndStreamMessage(buffer, flags)
		if err != nil {
			return err
		}
		hc.end = end
		return io.EOF
	}
	if err := unmarshal(buffer, msg, hc.codec); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *error is a non-nil error
}

func (hc *connectStreamingHandlerConn) RequestHeader() http.Header {
	return hc.request.Header
}

func (hc *connectStreamingHandlerConn) Send(msg any) error {
	if msg == nil {
		hc.responseWriter.WriteHeader(http.StatusOK)
		return nil
	}
	buffer := hc.bufferPool.Get()
	defer hc.bufferPool.Put(buffer)

	if err := marshal(buffer, msg, hc.codec); err != nil {
		return err
	}
	var flags uint8
	if buffer.Len() > hc.compressMinBytes && hc.compressionResponsePool != nil {
		if err := compress(buffer, hc.bufferPool, hc.compressionResponsePool); err != nil {
			return err
		}
		flags |= flagEnvelopeCompressed
	}
	if err := checkSendMaxBytes(buffer, hc.sendMaxBytes, flags&flagEnvelopeCompressed > 0); err != nil {
		return err
	}
	if err := writeEnvelope(hc.responseWriter, flags, buffer); err != nil {
		return err
	}
	flushResponseWriter(hc.responseWriter)
	return nil // must be a literal nil: nil *error is a non-nil error
}

func (hc *connectStreamingHandlerConn) ResponseHeader() http.Header {
	return hc.responseWriter.Header()
}

func (hc *connectStreamingHandlerConn) ResponseTrailer() http.Header {
	return hc.responseTrailer
}

func (hc *connectStreamingHandlerConn) Close(err error) error {
	defer flushResponseWriter(hc.responseWriter)

	if err := hc.marshalEndStream(err, hc.responseTrailer); err != nil {
		_ = hc.request.Body.Close()
		return err
	}
	// We don't want to copy unread portions of the body to /dev/null here: if
	// the client hasn't closed the request body, we'll block until the server
	// timeout kicks in. This could happen because the client is malicious, but
	// a well-intentioned client may just not expect the server to be returning
	// an error for a streaming RPC. Better to accept that we can't always reuse
	// TCP connections.
	if err := hc.request.Body.Close(); err != nil {
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		return NewError(CodeUnknown, err)
	}
	return nil // must be a literal nil: nil *Error is a non-nil error
}

func (hc *connectStreamingHandlerConn) marshalEndStream(err error, trailer http.Header) *Error {
	buffer := hc.bufferPool.Get()
	defer hc.bufferPool.Put(buffer)

	end := newConnectEndStreamMessage(err, trailer)
	if err := connectMarshalEndStreamMessage(buffer, end); err != nil {
		return err
	}
	return writeEnvelope(hc.responseWriter, connectFlagEnvelopeEndStream, buffer)
}

func (cc *connectUnaryClientConn) sendMsg(buffer *bytes.Buffer, msg any) error {
	if err := marshal(buffer, msg, cc.codec); err != nil {
		return err
	}
	var isCompressed bool
	if cc.compressionPool != nil && buffer.Len() > cc.compressMinBytes {
		if err := compress(buffer, cc.bufferPool, cc.compressionPool); err != nil {
			return err
		}
		header := cc.duplexCall.Header()
		setHeaderCanonical(header, connectUnaryHeaderCompression, cc.compressionName)
		isCompressed = true
	}
	if err := checkSendMaxBytes(buffer, cc.sendMaxBytes, isCompressed); err != nil {
		return err
	}
	if err := writeAll(cc.duplexCall, buffer); err != nil {
		return err
	}
	return nil
}

func (cc *connectUnaryClientConn) trySendGet(buffer *bytes.Buffer, msg any) error {
	if cc.stableCodec == nil {
		if cc.getUseFallback {
			return cc.sendMsg(buffer, msg)
		}
		return errorf(CodeInternal, "codec %s doesn't support stable marshal; can't use get", cc.codec.Name())
	}
	if msg != nil {
		data, err := cc.stableCodec.MarshalStable(msg)
		if err != nil {
			return err
		}
		setBuffer(buffer, data)
	}

	isTooBig := cc.sendMaxBytes > 0 && buffer.Len() > cc.sendMaxBytes
	isCompressed := false

	if isTooBig && cc.compressionPool != nil {
		if err := compress(buffer, cc.bufferPool, cc.compressionPool); err != nil {
			return err
		}
		isCompressed = true
		isTooBig = cc.sendMaxBytes > 0 && buffer.Len() > cc.sendMaxBytes
	}

	if isTooBig {
		if cc.getUseFallback {
			buffer.Reset()
			return cc.sendMsg(buffer, msg)
		}
		if isCompressed {
			return errorf(CodeResourceExhausted,
				"message size %d exceeds sendMaxBytes %d",
				buffer.Len(),
				cc.sendMaxBytes,
			)
		}
		return errorf(CodeResourceExhausted,
			"message size %d exceeds sendMaxBytes %d: enabling request compression may help",
			buffer.Len(),
			cc.sendMaxBytes,
		)
	}

	header := cc.duplexCall.Header()
	url := cc.buildGetURL(buffer.Bytes(), isCompressed)
	if cc.getURLMaxBytes > 0 && len(url.String()) > cc.getURLMaxBytes {
		if cc.getUseFallback {
			buffer.Reset()
			setHeaderCanonical(header, connectUnaryHeaderCompression, cc.compressionName)
			return cc.sendMsg(buffer, msg)
		}
		return errorf(CodeResourceExhausted,
			"compressed url size %d exceeds getURLMaxBytes %d",
			len(url.String()), cc.getURLMaxBytes)
	}

	delete(header, connectHeaderProtocolVersion)
	cc.duplexCall.SetMethod(http.MethodGet)
	*cc.duplexCall.URL() = *url
	return nil
}

func (cc *connectUnaryClientConn) buildGetURL(data []byte, compressed bool) *url.URL {
	url := *cc.duplexCall.URL()
	query := url.Query()
	query.Set(connectUnaryConnectQueryParameter, connectUnaryConnectQueryValue)
	query.Set(connectUnaryEncodingQueryParameter, cc.codec.Name())
	if cc.stableCodec.IsBinary() || compressed {
		query.Set(connectUnaryMessageQueryParameter, encodeBinaryQueryValue(data))
		query.Set(connectUnaryBase64QueryParameter, "1")
	} else {
		query.Set(connectUnaryMessageQueryParameter, string(data))
	}
	if compressed {
		query.Set(connectUnaryCompressionQueryParameter, cc.compressionName)
	}
	url.RawQuery = query.Encode()
	return &url
}

type connectWireDetail ErrorDetail

func (d *connectWireDetail) MarshalJSON() ([]byte, error) {
	if d.wireJSON != "" {
		// If we unmarshaled this detail from JSON, return the original data. This
		// lets proxies w/o protobuf descriptors preserve human-readable details.
		return []byte(d.wireJSON), nil
	}
	wire := struct {
		Type  string          `json:"type"`
		Value string          `json:"value"`
		Debug json.RawMessage `json:"debug,omitempty"`
	}{
		Type:  strings.TrimPrefix(d.pb.TypeUrl, defaultAnyResolverPrefix),
		Value: base64.RawStdEncoding.EncodeToString(d.pb.Value),
	}
	// Try to produce debug info, but expect failure when we don't have
	// descriptors.
	var codec protoJSONCodec
	debug, err := codec.Marshal(d.pb)
	if err == nil && len(debug) > 2 { // don't bother sending `{}`
		wire.Debug = json.RawMessage(debug)
	}
	return json.Marshal(wire)
}

func (d *connectWireDetail) UnmarshalJSON(data []byte) error {
	var wire struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if !strings.Contains(wire.Type, "/") {
		wire.Type = defaultAnyResolverPrefix + wire.Type
	}
	decoded, err := DecodeBinaryHeader(wire.Value)
	if err != nil {
		return fmt.Errorf("decode base64: %w", err)
	}
	*d = connectWireDetail{
		pb: &anypb.Any{
			TypeUrl: wire.Type,
			Value:   decoded,
		},
		wireJSON: string(data),
	}
	return nil
}

type connectWireError struct {
	Code    Code                 `json:"code"`
	Message string               `json:"message,omitempty"`
	Details []*connectWireDetail `json:"details,omitempty"`
}

func newConnectWireError(err error) *connectWireError {
	wire := &connectWireError{
		Code:    CodeUnknown,
		Message: err.Error(),
	}
	if connectErr, ok := asError(err); ok {
		wire.Code = connectErr.Code()
		wire.Message = connectErr.Message()
		if len(connectErr.details) > 0 {
			wire.Details = make([]*connectWireDetail, len(connectErr.details))
			for i, detail := range connectErr.details {
				wire.Details[i] = (*connectWireDetail)(detail)
			}
		}
	}
	return wire
}

func (e *connectWireError) asError() *Error {
	if e == nil {
		return nil
	}
	if e.Code < minCode || e.Code > maxCode {
		e.Code = CodeUnknown
	}
	err := NewWireError(e.Code, errors.New(e.Message))
	if len(e.Details) > 0 {
		err.details = make([]*ErrorDetail, len(e.Details))
		for i, detail := range e.Details {
			err.details[i] = (*ErrorDetail)(detail)
		}
	}
	return err
}

type connectEndStreamMessage struct {
	Error   *connectWireError `json:"error,omitempty"`
	Trailer http.Header       `json:"metadata,omitempty"`
}

func newConnectEndStreamMessage(err error, trailer http.Header) *connectEndStreamMessage {
	if trailer == nil {
		trailer = make(http.Header)
	}
	end := &connectEndStreamMessage{Trailer: trailer}
	if err != nil {
		end.Error = newConnectWireError(err)
		if connectErr, ok := asError(err); ok {
			mergeHeaders(end.Trailer, connectErr.meta)
		}
	}
	return end
}

func connectCodeToHTTP(code Code) int {
	// Return literals rather than named constants from the HTTP package to make
	// it easier to compare this function to the Connect specification.
	switch code {
	case CodeCanceled:
		return 408
	case CodeUnknown:
		return 500
	case CodeInvalidArgument:
		return 400
	case CodeDeadlineExceeded:
		return 408
	case CodeNotFound:
		return 404
	case CodeAlreadyExists:
		return 409
	case CodePermissionDenied:
		return 403
	case CodeResourceExhausted:
		return 429
	case CodeFailedPrecondition:
		return 412
	case CodeAborted:
		return 409
	case CodeOutOfRange:
		return 400
	case CodeUnimplemented:
		return 404
	case CodeInternal:
		return 500
	case CodeUnavailable:
		return 503
	case CodeDataLoss:
		return 500
	case CodeUnauthenticated:
		return 401
	default:
		return 500 // same as CodeUnknown
	}
}

func connectHTTPToCode(httpCode int) Code {
	// As above, literals are easier to compare to the specificaton (vs named
	// constants).
	switch httpCode {
	case 400:
		return CodeInvalidArgument
	case 401:
		return CodeUnauthenticated
	case 403:
		return CodePermissionDenied
	case 404:
		return CodeUnimplemented
	case 408:
		return CodeDeadlineExceeded
	case 412:
		return CodeFailedPrecondition
	case 413:
		return CodeResourceExhausted
	case 429:
		return CodeUnavailable
	case 431:
		return CodeResourceExhausted
	case 502, 503, 504:
		return CodeUnavailable
	default:
		return CodeUnknown
	}
}

func connectCodecFromContentType(streamType StreamType, contentType string) string {
	if streamType == StreamTypeUnary {
		return strings.TrimPrefix(contentType, connectUnaryContentTypePrefix)
	}
	return strings.TrimPrefix(contentType, connectStreamingContentTypePrefix)
}

func connectContentTypeFromCodecName(streamType StreamType, name string) string {
	if streamType == StreamTypeUnary {
		return connectUnaryContentTypePrefix + name
	}
	return connectStreamingContentTypePrefix + name
}

// encodeBinaryQueryValue URL-safe base64-encodes data, without padding.
func encodeBinaryQueryValue(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// binaryQueryValueReader creates a reader that can read either padded or
// unpadded URL-safe base64 from a string.
func binaryQueryValueReader(data string) io.Reader {
	stringReader := strings.NewReader(data)
	if len(data)%4 != 0 {
		// Data definitely isn't padded.
		return base64.NewDecoder(base64.RawURLEncoding, stringReader)
	}
	// Data is padded, or no padding was necessary.
	return base64.NewDecoder(base64.URLEncoding, stringReader)
}

// queryValueReader creates a reader for a string that may be URL-safe base64
// encoded.
func queryValueReader(data string, base64Encoded bool) io.Reader {
	if base64Encoded {
		return binaryQueryValueReader(data)
	}
	return strings.NewReader(data)
}

func connectUnmarshalEndStreamMessage(src *bytes.Buffer, flags uint8) (*connectEndStreamMessage, *Error) {
	end := connectEndStreamMessage{}
	if flags^connectFlagEnvelopeEndStream != 0 {
		return nil, newErrInvalidEnvelopeFlags(flags)
	}
	// end of stream...
	if err := json.Unmarshal(src.Bytes(), &end); err != nil {
		return nil, errorf(CodeInternal, "unmarshal end stream message: %w", err)
	}
	for name, value := range end.Trailer {
		canonical := http.CanonicalHeaderKey(name)
		if name != canonical {
			delete(end.Trailer, name)
			end.Trailer[canonical] = append(end.Trailer[canonical], value...)
		}
	}
	return &end, nil
}
func connectMarshalEndStreamMessage(dst *bytes.Buffer, end *connectEndStreamMessage) *Error {
	data, marshalErr := json.Marshal(end)
	if marshalErr != nil {
		return errorf(CodeInternal, "marshal end stream: %w", marshalErr)
	}
	setBuffer(dst, data)
	return nil
}
