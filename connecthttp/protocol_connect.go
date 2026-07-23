// Copyright 2021-2026 The Connect Authors
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

package connecthttp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/bufferpool"
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
	connectUnaryContentTypeJSON       = connectUnaryContentTypePrefix + connect.CodecNameJSON
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
var defaultConnectUserAgent = fmt.Sprintf("connect-go/%s (%s)", connect.Version, runtime.Version())

type protocolConnect struct{}

// NewHandler implements protocol, so it must return an interface.
func (*protocolConnect) NewHandler(params *protocolHandlerParams) protocolHandler {
	methods := make(map[string]struct{})
	methods[http.MethodPost] = struct{}{}

	if params.spec.StreamType == connect.StreamTypeUnary && params.IdempotencyLevel == connect.IdempotencyNoSideEffects {
		methods[http.MethodGet] = struct{}{}
	}

	contentTypes := make(map[string]struct{})
	for _, name := range params.Codecs.Names() {
		if params.spec.StreamType == connect.StreamTypeUnary {
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
		peer:                 newPeerForURL(params.URL, connect.ProtocolNameConnect),
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
		return nil, nil, connect.Errorf(connect.CodeInvalidArgument, "parse timeout: %q has >10 digits", timeout)
	}
	millis, err := strconv.ParseInt(timeout, 10 /* base */, 64 /* bitsize */)
	if err != nil {
		return nil, nil, connect.Errorf(connect.CodeInvalidArgument, "parse timeout: %s", err).WithCause(err)
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
		contentType = connectContentTypeForCodecName(
			h.spec.StreamType,
			codecName,
		)
	}
	_, ok := h.accept[contentType]
	return ok
}

func (h *connectHandler) NewConn(
	responseWriter http.ResponseWriter,
	request *http.Request,
	info *connect.CallInfo,
) (handlerConnCloser, bool) {
	ctx := request.Context()
	query := request.URL.Query()
	// We need to parse metadata before entering the interceptor stack; we'll
	// send the error to the client later on.
	var contentEncoding, acceptEncoding string
	if h.spec.StreamType == connect.StreamTypeUnary {
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
		failed = checkServerStreamsCanFlush(h.spec, responseWriter)
	}
	if failed == nil {
		required := h.RequireConnectProtocolHeader && (h.spec.StreamType == connect.StreamTypeUnary)
		failed = connectCheckProtocolVersion(request, required)
	}

	var requestBody io.ReadCloser
	var contentType, codecName string
	if request.Method == http.MethodGet {
		if failed == nil && !query.Has(connectUnaryEncodingQueryParameter) {
			failed = connect.Errorf(connect.CodeInvalidArgument, "missing %s parameter", connectUnaryEncodingQueryParameter)
		} else if failed == nil && !query.Has(connectUnaryMessageQueryParameter) {
			failed = connect.Errorf(connect.CodeInvalidArgument, "missing %s parameter", connectUnaryMessageQueryParameter)
		}
		msg := query.Get(connectUnaryMessageQueryParameter)
		msgReader := queryValueReader(msg, query.Get(connectUnaryBase64QueryParameter) == "1")
		requestBody = io.NopCloser(msgReader)
		codecName = query.Get(connectUnaryEncodingQueryParameter)
		contentType = connectContentTypeForCodecName(
			h.spec.StreamType,
			codecName,
		)
	} else {
		requestBody = request.Body
		contentType = getHeaderCanonical(request.Header, headerContentType)
		codecName = connectCodecForContentType(
			h.spec.StreamType,
			contentType,
		)
	}

	codec := h.Codecs.Get(codecName)
	// The codec can be nil in the GET request case; that's okay: when failed
	// is non-nil, codec is never used.
	if failed == nil && codec == nil {
		failed = connect.Errorf(connect.CodeInvalidArgument, "invalid message encoding: %q", codecName)
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
	if h.spec.StreamType != connect.StreamTypeUnary {
		acceptCompressionHeader = connectStreamingHeaderAcceptCompression
		// We only write the request encoding header here for streaming calls,
		// since the streaming envelope lets us choose whether to compress each
		// message individually. For unary, we won't know whether we're compressing
		// the request until we see how large the payload is.
		if responseCompression != connect.CompressionNameIdentity {
			header[connectStreamingHeaderCompression] = []string{responseCompression}
		}
	}
	header[acceptCompressionHeader] = []string{h.CompressionPools.CommaSeparatedNames()}

	var sendStats, receiveStats *connect.MessageStats
	if info != nil {
		info.Codec = codecName
		info.RequestEncoding = requestCompression
		info.ResponseEncoding = responseCompression
		sendStats, receiveStats = &info.SendStats, &info.ReceiveStats
	}
	var conn handlerConnCloser
	peer := peer{
		Addr:     request.RemoteAddr,
		Protocol: connect.ProtocolNameConnect,
		Query:    query,
	}
	if h.spec.StreamType == connect.StreamTypeUnary {
		conn = &connectUnaryHandlerConn{
			spec:           h.spec,
			peer:           peer,
			request:        request,
			responseWriter: responseWriter,
			marshaler: connectUnaryMarshaler{
				ctx:              ctx,
				sender:           writeSender{writer: responseWriter},
				codec:            codec,
				compressMinBytes: h.CompressMinBytes,
				compressionName:  responseCompression,
				compressionPool:  h.CompressionPools.Get(responseCompression),
				header:           responseWriter.Header(),
				sendMaxBytes:     h.SendMaxBytes,
				stats:            sendStats,
			},
			unmarshaler: connectUnaryUnmarshaler{
				ctx:             ctx,
				reader:          requestBody,
				codec:           codec,
				compressionPool: h.CompressionPools.Get(requestCompression),
				readMaxBytes:    h.ReadMaxBytes,
				stats:           receiveStats,
			},
			responseTrailer: make(http.Header),
		}
	} else {
		conn = &connectStreamingHandlerConn{
			spec:           h.spec,
			peer:           peer,
			request:        request,
			responseWriter: responseWriter,
			marshaler: connectStreamingMarshaler{
				envelopeWriter: envelopeWriter{
					ctx:              ctx,
					sender:           writeSender{responseWriter},
					codec:            codec,
					compressMinBytes: h.CompressMinBytes,
					compressionPool:  h.CompressionPools.Get(responseCompression),
					sendMaxBytes:     h.SendMaxBytes,
					stats:            sendStats,
				},
			},
			unmarshaler: connectStreamingUnmarshaler{
				envelopeReader: envelopeReader{
					ctx:             ctx,
					reader:          requestBody,
					codec:           codec,
					compressionPool: h.CompressionPools.Get(requestCompression),
					readMaxBytes:    h.ReadMaxBytes,
					stats:           receiveStats,
				},
			},
			responseTrailer: make(http.Header),
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

	peer peer
}

func (c *connectClient) Peer() peer {
	return c.peer
}

func (c *connectClient) WriteRequestHeader(streamType connect.StreamType, header http.Header) {
	setUserAgentIfAbsent(header, defaultConnectUserAgent)
	// We know these header keys are in canonical form, so we can bypass all the
	// checks in Header.Set.
	header[connectHeaderProtocolVersion] = []string{connectProtocolVersion}
	header[headerContentType] = []string{
		connectContentTypeForCodecName(streamType, c.Codec.Name()),
	}
	acceptCompressionHeader := connectUnaryHeaderAcceptCompression
	if streamType != connect.StreamTypeUnary {
		// If we don't set Accept-Encoding, by default http.Client will ask the
		// server to compress the whole stream. Since we're already compressing
		// each message, this is a waste.
		header[connectUnaryHeaderAcceptCompression] = []string{connect.CompressionNameIdentity}
		acceptCompressionHeader = connectStreamingHeaderAcceptCompression
		// We only write the request encoding header here for streaming calls,
		// since the streaming envelope lets us choose whether to compress each
		// message individually. For unary, we won't know whether we're compressing
		// the request until we see how large the payload is.
		if c.CompressionName != "" && c.CompressionName != connect.CompressionNameIdentity {
			header[connectStreamingHeaderCompression] = []string{c.CompressionName}
		}
	}
	if acceptCompression := c.CompressionPools.CommaSeparatedNames(); acceptCompression != "" {
		header[acceptCompressionHeader] = []string{acceptCompression}
	}
}

func (c *connectClient) NewConn(
	ctx context.Context,
	spec connect.Spec,
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
	duplexCall := newDuplexHTTPCall(ctx, c.HTTPClient, c.URL, spec.StreamType, header)
	info, ok := connect.CallInfoForClientContext(ctx)
	var sendStats, receiveStats *connect.MessageStats
	if ok {
		sendStats, receiveStats = &info.SendStats, &info.ReceiveStats
	}
	var conn streamingClientConn
	if spec.StreamType == connect.StreamTypeUnary {
		unaryConn := &connectUnaryClientConn{
			spec:             spec,
			peer:             c.Peer(),
			info:             info,
			duplexCall:       duplexCall,
			compressionPools: c.CompressionPools,
			marshaler: connectUnaryRequestMarshaler{
				connectUnaryMarshaler: connectUnaryMarshaler{
					ctx:              ctx,
					sender:           duplexCall,
					codec:            c.Codec,
					compressMinBytes: c.CompressMinBytes,
					compressionName:  c.CompressionName,
					compressionPool:  c.CompressionPools.Get(c.CompressionName),
					header:           duplexCall.Header(),
					sendMaxBytes:     c.SendMaxBytes,
					stats:            sendStats,
				},
			},
			unmarshaler: connectUnaryUnmarshaler{
				ctx:          ctx,
				reader:       duplexCall,
				codec:        c.Codec,
				readMaxBytes: c.ReadMaxBytes,
				stats:        receiveStats,
			},
			responseHeader:  make(http.Header),
			responseTrailer: make(http.Header),
		}
		if spec.IdempotencyLevel == connect.IdempotencyNoSideEffects {
			unaryConn.marshaler.enableGet = c.EnableGet
			unaryConn.marshaler.getURLMaxBytes = c.GetURLMaxBytes
			unaryConn.marshaler.getUseFallback = c.GetUseFallback
			unaryConn.marshaler.duplexCall = duplexCall
			if stableCodec, ok := c.Codec.(connect.StableCodec); ok {
				unaryConn.marshaler.stableCodec = stableCodec
			}
		}
		conn = unaryConn
		duplexCall.SetValidateResponse(unaryConn.validateResponse)
	} else {
		streamingConn := &connectStreamingClientConn{
			spec:             spec,
			peer:             c.Peer(),
			info:             info,
			duplexCall:       duplexCall,
			compressionPools: c.CompressionPools,
			codec:            c.Codec,
			marshaler: connectStreamingMarshaler{
				envelopeWriter: envelopeWriter{
					ctx:              ctx,
					sender:           duplexCall,
					codec:            c.Codec,
					compressMinBytes: c.CompressMinBytes,
					compressionPool:  c.CompressionPools.Get(c.CompressionName),
					sendMaxBytes:     c.SendMaxBytes,
					stats:            sendStats,
				},
			},
			unmarshaler: connectStreamingUnmarshaler{
				envelopeReader: envelopeReader{
					ctx:          ctx,
					reader:       duplexCall,
					codec:        c.Codec,
					readMaxBytes: c.ReadMaxBytes,
					stats:        receiveStats,
				},
			},
			responseHeader:  make(http.Header),
			responseTrailer: make(http.Header),
		}
		conn = streamingConn
		duplexCall.SetValidateResponse(streamingConn.validateResponse)
	}
	return wrapClientConnWithCodedErrors(conn)
}

type connectUnaryClientConn struct {
	spec             connect.Spec
	peer             peer
	info             *connect.CallInfo
	duplexCall       *duplexHTTPCall
	compressionPools readOnlyCompressionPools
	marshaler        connectUnaryRequestMarshaler
	unmarshaler      connectUnaryUnmarshaler
	responseHeader   http.Header
	responseTrailer  http.Header
}

func (cc *connectUnaryClientConn) Spec() connect.Spec {
	return cc.spec
}

func (cc *connectUnaryClientConn) Peer() peer {
	return cc.peer
}

func (cc *connectUnaryClientConn) Send(msg any) error {
	if err := cc.marshaler.Marshal(msg); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *connect.Error is a non-nil error
}

func (cc *connectUnaryClientConn) RequestHeader() http.Header {
	return cc.duplexCall.Header()
}

func (cc *connectUnaryClientConn) CloseRequest() error {
	return cc.duplexCall.CloseWrite()
}

func (cc *connectUnaryClientConn) Receive(msg any) error {
	if _, err := cc.duplexCall.blockUntilResponseReady(); err != nil {
		return err
	}
	if err := cc.unmarshaler.Unmarshal(msg); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *connect.Error is a non-nil error
}

func (cc *connectUnaryClientConn) ResponseHeader() http.Header {
	if cc.duplexCall.awaitResponse() {
		return cc.responseHeader
	}
	return make(http.Header)
}

func (cc *connectUnaryClientConn) ResponseTrailer() http.Header {
	if cc.duplexCall.awaitResponse() {
		return cc.responseTrailer
	}
	return make(http.Header)
}

func (cc *connectUnaryClientConn) CloseResponse() error {
	return cc.duplexCall.CloseRead()
}

func (cc *connectUnaryClientConn) onRequestSend(fn func(*http.Request)) {
	cc.duplexCall.onRequestSend = fn
}

func (cc *connectUnaryClientConn) onResponseReceive(fn func(*http.Response)) {
	cc.duplexCall.onResponseReceive = fn
}

func (cc *connectUnaryClientConn) validateResponse(response *http.Response) *connect.Error {
	for k, v := range response.Header {
		if !strings.HasPrefix(k, connectUnaryTrailerPrefix) {
			cc.responseHeader[k] = v
			continue
		}
		cc.responseTrailer[k[len(connectUnaryTrailerPrefix):]] = v
	}
	if err := connectValidateUnaryResponseContentType(
		cc.marshaler.codec.Name(),
		cc.duplexCall.Method(),
		response.StatusCode,
		response.Status,
		getHeaderCanonical(response.Header, headerContentType),
	); err != nil {
		return err
	}
	compression := getHeaderCanonical(response.Header, connectUnaryHeaderCompression)
	if compression != "" &&
		compression != connect.CompressionNameIdentity &&
		!cc.compressionPools.Contains(compression) {
		return connect.Errorf(
			connect.CodeInternal,
			"unknown encoding %q: accepted encodings are %v",
			compression,
			cc.compressionPools.CommaSeparatedNames(),
		)
	}
	cc.unmarshaler.compressionPool = cc.compressionPools.Get(compression)
	if cc.info != nil {
		cc.info.ResponseEncoding = encodingOrIdentity(compression)
	}
	if response.StatusCode != http.StatusOK {
		unmarshaler := connectUnaryUnmarshaler{
			ctx:             cc.unmarshaler.ctx,
			reader:          response.Body,
			compressionPool: cc.unmarshaler.compressionPool,
		}
		var wireErr connectWireError
		jsonUnmarshaller := func(_ context.Context, src io.Reader, msg any) error {
			return json.NewDecoder(src).Decode(msg)
		}
		if err := unmarshaler.UnmarshalFunc(&wireErr, jsonUnmarshaller); err != nil {
			return connect.NewError(
				httpToCode(response.StatusCode),
				response.Status,
			)
		}
		if wireErr.Code == 0 {
			// code not set? default to one implied by HTTP status
			wireErr.Code = httpToCode(response.StatusCode)
		}
		serverErr := wireErr.asError()
		if serverErr == nil {
			return nil
		}
		return serverErr
	}
	return nil
}

type connectStreamingClientConn struct {
	spec             connect.Spec
	peer             peer
	info             *connect.CallInfo
	duplexCall       *duplexHTTPCall
	compressionPools readOnlyCompressionPools
	codec            connect.Codec
	marshaler        connectStreamingMarshaler
	unmarshaler      connectStreamingUnmarshaler
	responseHeader   http.Header
	responseTrailer  http.Header
}

func (cc *connectStreamingClientConn) Spec() connect.Spec {
	return cc.spec
}

func (cc *connectStreamingClientConn) Peer() peer {
	return cc.peer
}

func (cc *connectStreamingClientConn) Send(msg any) error {
	if err := cc.marshaler.Marshal(msg); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *connect.Error is a non-nil error
}

func (cc *connectStreamingClientConn) RequestHeader() http.Header {
	return cc.duplexCall.Header()
}

func (cc *connectStreamingClientConn) CloseRequest() error {
	return cc.duplexCall.CloseWrite()
}

func (cc *connectStreamingClientConn) Receive(msg any) error {
	if _, err := cc.duplexCall.blockUntilResponseReady(); err != nil {
		return err
	}
	err := cc.unmarshaler.Unmarshal(msg)
	if err == nil {
		return nil
	}
	// See if the server sent an explicit error in the end-of-stream message.
	mergeHeaders(cc.responseTrailer, cc.unmarshaler.Trailer())
	if serverErr := cc.unmarshaler.EndStreamError(); serverErr != nil {
		// This is expected from a protocol perspective, but receiving an
		// end-of-stream message means that we're _not_ getting a regular message.
		// For users to realize that the stream has ended, Receive must return an
		// error.
		_ = cc.duplexCall.CloseWrite()
		return serverErr
	}
	// If the error is EOF but not from a last message, we want to return
	// io.ErrUnexpectedEOF instead.
	if errors.Is(err, io.EOF) && !errors.Is(err, errSpecialEnvelope) {
		err = connect.Errorf(connect.CodeInternal, "protocol error: %s", io.ErrUnexpectedEOF).WithCause(io.ErrUnexpectedEOF)
	}
	// There's no error in the trailers, so this was probably an error
	// converting the bytes to a message, an error reading from the network, or
	// just an EOF. We're going to return it to the user, but we also want to
	// close the writer so Send errors out.
	_ = cc.duplexCall.CloseWrite()
	return err
}

func (cc *connectStreamingClientConn) ResponseHeader() http.Header {
	if cc.duplexCall.awaitResponse() {
		return cc.responseHeader
	}
	return make(http.Header)
}

func (cc *connectStreamingClientConn) ResponseTrailer() http.Header {
	if cc.duplexCall.awaitResponse() {
		return cc.responseTrailer
	}
	return make(http.Header)
}

func (cc *connectStreamingClientConn) CloseResponse() error {
	return cc.duplexCall.CloseRead()
}

func (cc *connectStreamingClientConn) onRequestSend(fn func(*http.Request)) {
	cc.duplexCall.onRequestSend = fn
}

func (cc *connectStreamingClientConn) onResponseReceive(fn func(*http.Response)) {
	cc.duplexCall.onResponseReceive = fn
}

func (cc *connectStreamingClientConn) validateResponse(response *http.Response) *connect.Error {
	if response.StatusCode != http.StatusOK {
		return connect.Errorf(httpToCode(response.StatusCode), "HTTP status %v", response.Status)
	}
	if err := connectValidateStreamResponseContentType(
		cc.codec.Name(),
		cc.spec.StreamType,
		getHeaderCanonical(response.Header, headerContentType),
	); err != nil {
		return err
	}
	compression := getHeaderCanonical(response.Header, connectStreamingHeaderCompression)
	if compression != "" &&
		compression != connect.CompressionNameIdentity &&
		!cc.compressionPools.Contains(compression) {
		return connect.Errorf(
			connect.CodeInternal,
			"unknown encoding %q: accepted encodings are %v",
			compression,
			cc.compressionPools.CommaSeparatedNames(),
		)
	}
	cc.unmarshaler.compressionPool = cc.compressionPools.Get(compression)
	if cc.info != nil {
		cc.info.ResponseEncoding = encodingOrIdentity(compression)
	}
	mergeHeaders(cc.responseHeader, response.Header)
	return nil
}

type connectUnaryHandlerConn struct {
	spec            connect.Spec
	peer            peer
	request         *http.Request
	responseWriter  http.ResponseWriter
	marshaler       connectUnaryMarshaler
	unmarshaler     connectUnaryUnmarshaler
	responseTrailer http.Header
}

func (hc *connectUnaryHandlerConn) Spec() connect.Spec {
	return hc.spec
}

func (hc *connectUnaryHandlerConn) Peer() peer {
	return hc.peer
}

func (hc *connectUnaryHandlerConn) Receive(msg any) error {
	if err := hc.unmarshaler.Unmarshal(msg); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *connect.Error is a non-nil error
}

func (hc *connectUnaryHandlerConn) RequestHeader() http.Header {
	return hc.request.Header
}

func (hc *connectUnaryHandlerConn) Send(msg any) error {
	hc.mergeResponseHeader()
	if err := hc.marshaler.Marshal(msg); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *connect.Error is a non-nil error
}

func (hc *connectUnaryHandlerConn) ResponseHeader() http.Header {
	return hc.responseWriter.Header()
}

func (hc *connectUnaryHandlerConn) ResponseTrailer() http.Header {
	return hc.responseTrailer
}

func (hc *connectUnaryHandlerConn) Close(err error) error {
	if !hc.marshaler.wroteHeader {
		hc.mergeResponseHeader()
		// If the handler received a GET request and the resource hasn't changed,
		// return a 304.
		if len(hc.peer.Query) > 0 && IsNotModifiedError(err) {
			hc.responseWriter.WriteHeader(http.StatusNotModified)
			return hc.request.Body.Close()
		}
	}
	if err == nil || hc.marshaler.wroteHeader {
		return hc.request.Body.Close()
	}
	// In unary Connect, errors always use application/json.
	setHeaderCanonical(hc.responseWriter.Header(), headerContentType, connectUnaryContentTypeJSON)
	hc.responseWriter.WriteHeader(connectCodeToHTTP(connect.CodeOf(err)))
	data, marshalErr := json.Marshal(newConnectWireError(err))
	if marshalErr != nil {
		_ = hc.request.Body.Close()
		return connect.Errorf(connect.CodeInternal, "marshal error: %s", err).WithCause(err)
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

func (hc *connectUnaryHandlerConn) mergeResponseHeader() {
	header := hc.responseWriter.Header()
	if hc.request.Method == http.MethodGet {
		// The response content varies depending on the compression that the client
		// requested (if any). GETs are potentially cacheable, so we should ensure
		// that the Vary header includes at least Accept-Encoding (and not overwrite any values already set).
		header[headerVary] = append(header[headerVary], connectUnaryHeaderAcceptCompression)
	}
	for k, v := range hc.responseTrailer {
		header[connectUnaryTrailerPrefix+k] = v
	}
}

type connectStreamingHandlerConn struct {
	spec            connect.Spec
	peer            peer
	request         *http.Request
	responseWriter  http.ResponseWriter
	marshaler       connectStreamingMarshaler
	unmarshaler     connectStreamingUnmarshaler
	responseTrailer http.Header
}

func (hc *connectStreamingHandlerConn) Spec() connect.Spec {
	return hc.spec
}

func (hc *connectStreamingHandlerConn) Peer() peer {
	return hc.peer
}

func (hc *connectStreamingHandlerConn) Receive(msg any) error {
	if err := hc.unmarshaler.Unmarshal(msg); err != nil {
		// Clients may not send end-of-stream metadata, so we don't need to handle
		// errSpecialEnvelope.
		return err
	}
	return nil // must be a literal nil: nil *connect.Error is a non-nil error
}

func (hc *connectStreamingHandlerConn) RequestHeader() http.Header {
	return hc.request.Header
}

func (hc *connectStreamingHandlerConn) Send(msg any) error {
	defer flushResponseWriter(hc.responseWriter)
	if err := hc.marshaler.Marshal(msg); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *connect.Error is a non-nil error
}

func (hc *connectStreamingHandlerConn) ResponseHeader() http.Header {
	return hc.responseWriter.Header()
}

func (hc *connectStreamingHandlerConn) ResponseTrailer() http.Header {
	return hc.responseTrailer
}

func (hc *connectStreamingHandlerConn) Close(err error) error {
	defer flushResponseWriter(hc.responseWriter)
	if err := hc.marshaler.MarshalEndStream(err, hc.responseTrailer); err != nil {
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
		return connect.Errorf(connect.CodeUnknown, "%s", err).WithCause(err)
	}
	return nil // must be a literal nil: nil *connect.Error is a non-nil error
}

type connectStreamingMarshaler struct {
	envelopeWriter
}

func (m *connectStreamingMarshaler) MarshalEndStream(err error, trailer http.Header) *connect.Error {
	end := &connectEndStreamMessage{Trailer: trailer}
	if err != nil {
		end.Error = newConnectWireError(err)
	}
	data, marshalErr := json.Marshal(end)
	if marshalErr != nil {
		return connect.Errorf(connect.CodeInternal, "marshal end stream: %s", marshalErr).WithCause(marshalErr)
	}
	raw := bytes.NewBuffer(data)
	defer bufferpool.Put(raw)
	return m.Write(&envelope{
		Data:  raw,
		Flags: connectFlagEnvelopeEndStream,
	})
}

type connectStreamingUnmarshaler struct {
	envelopeReader

	endStreamErr *connect.Error
	trailer      http.Header
}

func (u *connectStreamingUnmarshaler) Unmarshal(message any) *connect.Error {
	err := u.envelopeReader.Unmarshal(message)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errSpecialEnvelope) {
		return err
	}
	env := u.last
	data := env.Data
	u.last.Data = nil // don't keep a reference to it
	defer bufferpool.Put(data)
	if !env.IsSet(connectFlagEnvelopeEndStream) {
		return connect.Errorf(connect.CodeInternal, "protocol error: invalid envelope flags %d", env.Flags)
	}
	var end connectEndStreamMessage
	if err := json.Unmarshal(data.Bytes(), &end); err != nil {
		return connect.Errorf(connect.CodeInternal, "unmarshal end stream message: %s", err).WithCause(err)
	}
	for name, value := range end.Trailer {
		canonical := http.CanonicalHeaderKey(name)
		if name != canonical {
			delHeaderCanonical(end.Trailer, name)
			end.Trailer[canonical] = append(end.Trailer[canonical], value...)
		}
	}
	u.trailer = end.Trailer
	u.endStreamErr = end.Error.asError()
	return errSpecialEnvelope
}

func (u *connectStreamingUnmarshaler) Trailer() http.Header {
	return u.trailer
}

func (u *connectStreamingUnmarshaler) EndStreamError() *connect.Error {
	return u.endStreamErr
}

type connectUnaryMarshaler struct {
	ctx              context.Context //nolint:containedctx
	sender           messageSender
	codec            connect.Codec
	compressMinBytes int
	compressionName  string
	compressionPool  *compressionPool
	header           http.Header
	sendMaxBytes     int
	wroteHeader      bool
	stats            *connect.MessageStats
}

func (m *connectUnaryMarshaler) recordStats(size, compressedSize int) {
	if m.stats == nil {
		return
	}
	*m.stats = connect.MessageStats{Size: size, CompressedSize: compressedSize}
}

func (m *connectUnaryMarshaler) Marshal(message any) *connect.Error {
	if message == nil {
		return m.write(nil)
	}
	uncompressed := bufferpool.Get()
	if err := m.codec.MarshalWrite(m.ctx, uncompressed, message); err != nil {
		return connect.Errorf(connect.CodeInternal, "marshal message: %s", err).WithCause(err)
	}
	defer bufferpool.Put(uncompressed)
	if uncompressed.Len() < m.compressMinBytes || m.compressionPool == nil {
		if m.sendMaxBytes > 0 && uncompressed.Len() > m.sendMaxBytes {
			return connect.Errorf(connect.CodeResourceExhausted, "message size %d exceeds sendMaxBytes %d", uncompressed.Len(), m.sendMaxBytes)
		}
		if err := m.write(uncompressed.Bytes()); err != nil {
			return err
		}
		m.recordStats(uncompressed.Len(), 0)
		return nil
	}
	size := uncompressed.Len() // before Compress drains the buffer
	compressed := bufferpool.Get()
	defer bufferpool.Put(compressed)
	if err := m.compressionPool.Compress(compressed, uncompressed); err != nil {
		return err
	}
	if m.sendMaxBytes > 0 && compressed.Len() > m.sendMaxBytes {
		return connect.Errorf(connect.CodeResourceExhausted, "compressed message size %d exceeds sendMaxBytes %d", compressed.Len(), m.sendMaxBytes)
	}
	setHeaderCanonical(m.header, connectUnaryHeaderCompression, m.compressionName)
	if err := m.write(compressed.Bytes()); err != nil {
		return err
	}
	m.recordStats(size, compressed.Len())
	return nil
}

func (m *connectUnaryMarshaler) write(data []byte) *connect.Error {
	m.wroteHeader = true
	payload := bytes.NewReader(data)
	if _, err := m.sender.Send(payload); err != nil {
		err = wrapIfContextError(err)
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		return connect.Errorf(connect.CodeUnknown, "write message: %s", err).WithCause(err)
	}
	return nil
}

func (m *connectUnaryMarshaler) writeAndRecord(data []byte, size, compressedSize int) *connect.Error {
	if err := m.write(data); err != nil {
		return err
	}
	m.recordStats(size, compressedSize)
	return nil
}

type connectUnaryRequestMarshaler struct {
	connectUnaryMarshaler

	enableGet      bool
	getURLMaxBytes int
	getUseFallback bool
	stableCodec    connect.StableCodec
	duplexCall     *duplexHTTPCall
}

func (m *connectUnaryRequestMarshaler) Marshal(message any) *connect.Error {
	if m.enableGet {
		if m.stableCodec == nil && !m.getUseFallback {
			return connect.Errorf(connect.CodeInternal, "codec %s doesn't support stable marshal; can't use get", m.codec.Name())
		}
		if m.stableCodec != nil {
			return m.marshalWithGet(message)
		}
	}
	return m.connectUnaryMarshaler.Marshal(message)
}

func (m *connectUnaryRequestMarshaler) marshalWithGet(message any) *connect.Error {
	var buffer bytes.Buffer
	var err error
	if message != nil {
		if err = m.stableCodec.MarshalWriteStable(context.Background(), &buffer, message); err != nil {
			return connect.Errorf(connect.CodeInternal, "marshal message stable: %s", err).WithCause(err)
		}
	}
	isTooBig := m.sendMaxBytes > 0 && buffer.Len() > m.sendMaxBytes
	if isTooBig && m.compressionPool == nil {
		return connect.Errorf(connect.CodeResourceExhausted,
			"message size %d exceeds sendMaxBytes %d: enabling request compression may help",
			buffer.Len(),
			m.sendMaxBytes,
		)
	}
	data := buffer.Bytes()
	if !isTooBig {
		url := m.buildGetURL(data, false /* compressed */)
		if m.getURLMaxBytes <= 0 || len(url.String()) < m.getURLMaxBytes {
			m.writeWithGet(url)
			m.recordStats(len(data), 0)
			return nil
		}
		if m.compressionPool == nil {
			if m.getUseFallback {
				return m.writeAndRecord(data, len(data), 0)
			}
			return connect.Errorf(connect.CodeResourceExhausted,
				"url size %d exceeds getURLMaxBytes %d: enabling request compression may help",
				len(url.String()),
				m.getURLMaxBytes,
			)
		}
	}
	// Compress message to try to make it fit in the URL.
	uncompressed := bytes.NewBuffer(data)
	defer bufferpool.Put(uncompressed)
	compressed := bufferpool.Get()
	defer bufferpool.Put(compressed)
	if err := m.compressionPool.Compress(compressed, uncompressed); err != nil {
		return err
	}
	if m.sendMaxBytes > 0 && compressed.Len() > m.sendMaxBytes {
		return connect.Errorf(connect.CodeResourceExhausted, "compressed message size %d exceeds sendMaxBytes %d", compressed.Len(), m.sendMaxBytes)
	}
	url := m.buildGetURL(compressed.Bytes(), true /* compressed */)
	if m.getURLMaxBytes <= 0 || len(url.String()) < m.getURLMaxBytes {
		m.writeWithGet(url)
		m.recordStats(len(data), compressed.Len())
		return nil
	}
	if m.getUseFallback {
		setHeaderCanonical(m.header, connectUnaryHeaderCompression, m.compressionName)
		return m.writeAndRecord(compressed.Bytes(), len(data), compressed.Len())
	}
	return connect.Errorf(connect.CodeResourceExhausted, "compressed url size %d exceeds getURLMaxBytes %d", len(url.String()), m.getURLMaxBytes)
}

func (m *connectUnaryRequestMarshaler) buildGetURL(data []byte, compressed bool) *url.URL {
	var query strings.Builder
	appendQueryParam(&query, false, connectUnaryConnectQueryParameter, connectUnaryConnectQueryValue)
	binary := m.stableCodec.IsBinary() || compressed
	if binary {
		appendQueryParam(&query, true, connectUnaryBase64QueryParameter, "1")
	}
	if compressed {
		appendQueryParam(&query, true, connectUnaryCompressionQueryParameter, url.QueryEscape(m.compressionName))
	}
	appendQueryParam(&query, true, connectUnaryEncodingQueryParameter, url.QueryEscape(m.codec.Name()))
	if binary {
		appendQueryParam(&query, true, connectUnaryMessageQueryParameter, encodeBinaryQueryValue(data))
	} else {
		appendQueryParam(&query, true, connectUnaryMessageQueryParameter, url.QueryEscape(string(data)))
	}
	target := *m.duplexCall.URL()
	target.RawQuery = query.String()
	return &target
}

func appendQueryParam(query *strings.Builder, withSeparator bool, key, escapedValue string) {
	if withSeparator {
		query.WriteByte('&')
	}
	query.WriteString(key)
	query.WriteByte('=')
	query.WriteString(escapedValue)
}

func (m *connectUnaryRequestMarshaler) writeWithGet(url *url.URL) {
	delHeaderCanonical(m.header, connectHeaderProtocolVersion)
	delHeaderCanonical(m.header, headerContentType)
	delHeaderCanonical(m.header, headerContentEncoding)
	delHeaderCanonical(m.header, headerContentLength)
	m.duplexCall.SetMethod(http.MethodGet)
	*m.duplexCall.URL() = *url
}

type connectUnaryUnmarshaler struct {
	ctx             context.Context //nolint:containedctx
	reader          io.Reader
	codec           connect.Codec
	compressionPool *compressionPool
	alreadyRead     bool
	readMaxBytes    int
	stats           *connect.MessageStats
}

func (u *connectUnaryUnmarshaler) Unmarshal(message any) *connect.Error {
	return u.UnmarshalFunc(message, u.codec.UnmarshalRead)
}

func (u *connectUnaryUnmarshaler) UnmarshalFunc(message any, unmarshal func(context.Context, io.Reader, any) error) *connect.Error {
	if u.alreadyRead {
		return connect.Errorf(connect.CodeInternal, "%s", io.EOF).WithCause(io.EOF)
	}
	u.alreadyRead = true
	data := bufferpool.Get()
	defer bufferpool.Put(data)
	reader := u.reader
	if u.readMaxBytes > 0 && int64(u.readMaxBytes) < math.MaxInt64 {
		reader = io.LimitReader(u.reader, int64(u.readMaxBytes)+1)
	}
	// ReadFor ignores io.EOF, so any error here is real.
	bytesRead, err := data.ReadFrom(reader)
	if err != nil {
		err = wrapIfMaxBytesError(err, "read first %d bytes of message", bytesRead)
		err = wrapIfContextDone(u.ctx, err)
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		return connect.Errorf(connect.CodeUnknown, "read message: %s", err).WithCause(err)
	}
	if u.readMaxBytes > 0 && bytesRead > int64(u.readMaxBytes) {
		// Attempt to read to end in order to allow connection re-use
		discardedBytes, err := io.Copy(io.Discard, u.reader)
		if err != nil {
			return connect.Errorf(connect.CodeResourceExhausted, "message is larger than configured max %d - unable to determine message size: %s", u.readMaxBytes, err).WithCause(err)
		}
		return connect.Errorf(connect.CodeResourceExhausted, "message size %d is larger than configured max %d", bytesRead+discardedBytes, u.readMaxBytes)
	}
	compressedSize := 0
	if data.Len() > 0 && u.compressionPool != nil {
		compressedSize = data.Len()
		decompressed := bufferpool.Get()
		defer bufferpool.Put(decompressed)
		if err := u.compressionPool.Decompress(decompressed, data, int64(u.readMaxBytes)); err != nil {
			return err
		}
		data = decompressed
	}
	size := data.Len() // before unmarshal drains the buffer
	if err := unmarshal(u.ctx, data, message); err != nil {
		return connect.Errorf(connect.CodeInvalidArgument, "unmarshal message: %s", err).WithCause(err)
	}
	if u.stats != nil {
		*u.stats = connect.MessageStats{Size: size, CompressedSize: compressedSize}
	}
	return nil
}

// connectWireDetail adapts a [connect.ErrorDetail] to the Connect protocol's
// error detail object.
type connectWireDetail connect.ErrorDetail

func (d *connectWireDetail) MarshalJSON() ([]byte, error) {
	wire := struct {
		Type  string          `json:"type"`
		Value string          `json:"value"`
		Debug json.RawMessage `json:"debug,omitempty"`
	}{
		Type:  d.Type,
		Value: base64.RawStdEncoding.EncodeToString(d.Value),
	}
	if json.Valid(d.Debug) {
		wire.Debug = json.RawMessage(d.Debug)
	} else if msg, err := connectproto.ErrorDetailToAny((*connect.ErrorDetail)(d)).UnmarshalNew(); err == nil {
		var buffer bytes.Buffer
		var codec connectproto.JSONCodec
		if err := codec.MarshalWrite(context.Background(), &buffer, msg); err == nil {
			wire.Debug = buffer.Bytes()
		}
	}
	return json.Marshal(wire)
}

func (d *connectWireDetail) UnmarshalJSON(data []byte) error {
	var wire struct {
		Type  string          `json:"type"`
		Value string          `json:"value"`
		Debug json.RawMessage `json:"debug,omitempty"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	value, err := connect.DecodeBinaryHeader(wire.Value)
	if err != nil {
		return fmt.Errorf("decode base64: %w", err)
	}
	*d = connectWireDetail{
		Type:  wire.Type,
		Value: value,
		Debug: wire.Debug,
	}
	return nil
}

type connectWireError struct {
	Code    connect.Code         `json:"code"`
	Message string               `json:"message,omitempty"`
	Details []*connectWireDetail `json:"details,omitempty"`
}

func newConnectWireError(err error) *connectWireError {
	wire := &connectWireError{
		Code:    connect.CodeUnknown,
		Message: err.Error(),
	}
	if connectErr, ok := asError(err); ok {
		wire.Code = connectErr.Code()
		wire.Message = connectErr.Message()
		if details := connectErr.Details(); len(details) > 0 {
			wire.Details = make([]*connectWireDetail, len(details))
			for i, detail := range details {
				wire.Details[i] = (*connectWireDetail)(detail)
			}
		}
	}
	return wire
}

func (e *connectWireError) asError() *connect.Error {
	if e == nil {
		return nil
	}
	if e.Code < connect.CodeCanceled || e.Code > connect.CodeUnauthenticated {
		e.Code = connect.CodeUnknown
	}
	err := connect.NewError(e.Code, e.Message).WithRemote()
	if len(e.Details) > 0 {
		for _, detail := range e.Details {
			err = err.WithDetail((*connect.ErrorDetail)(detail))
		}
	}
	return err
}

func (e *connectWireError) UnmarshalJSON(data []byte) error {
	// We want to be lenient if the JSON has an unrecognized or invalid code.
	// So if that occurs, we leave the code unset but can still de-serialize
	// the other fields from the input JSON.
	var wireError struct {
		Code    string               `json:"code"`
		Message string               `json:"message"`
		Details []*connectWireDetail `json:"details"`
	}
	err := json.Unmarshal(data, &wireError)
	if err != nil {
		return err
	}
	e.Message = wireError.Message
	e.Details = wireError.Details
	// This will leave e.Code unset if we can't unmarshal the given string.
	_ = e.Code.UnmarshalText([]byte(wireError.Code))
	return nil
}

type connectEndStreamMessage struct {
	Error   *connectWireError `json:"error,omitempty"`
	Trailer http.Header       `json:"metadata,omitempty"`
}

func connectCodeToHTTP(code connect.Code) int {
	// Return literals rather than named constants from the HTTP package to make
	// it easier to compare this function to the Connect specification.
	switch code {
	case connect.CodeCanceled:
		return 499
	case connect.CodeUnknown:
		return 500
	case connect.CodeInvalidArgument:
		return 400
	case connect.CodeDeadlineExceeded:
		return 504
	case connect.CodeNotFound:
		return 404
	case connect.CodeAlreadyExists:
		return 409
	case connect.CodePermissionDenied:
		return 403
	case connect.CodeResourceExhausted:
		return 429
	case connect.CodeFailedPrecondition:
		return 400
	case connect.CodeAborted:
		return 409
	case connect.CodeOutOfRange:
		return 400
	case connect.CodeUnimplemented:
		return 501
	case connect.CodeInternal:
		return 500
	case connect.CodeUnavailable:
		return 503
	case connect.CodeDataLoss:
		return 500
	case connect.CodeUnauthenticated:
		return 401
	default:
		return 500 // same as connect.CodeUnknown
	}
}

func connectCodecForContentType(streamType connect.StreamType, contentType string) string {
	if streamType == connect.StreamTypeUnary {
		return strings.TrimPrefix(contentType, connectUnaryContentTypePrefix)
	}
	return strings.TrimPrefix(contentType, connectStreamingContentTypePrefix)
}

func connectContentTypeForCodecName(streamType connect.StreamType, name string) string {
	if streamType == connect.StreamTypeUnary {
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

func connectValidateUnaryResponseContentType(
	requestCodecName string,
	httpMethod string,
	statusCode int,
	statusMsg string,
	responseContentType string,
) *connect.Error {
	if statusCode != http.StatusOK {
		if statusCode == http.StatusNotModified && httpMethod == http.MethodGet {
			return connect.Errorf(connect.CodeUnknown, "%s", errNotModifiedClient).WithCause(errNotModifiedClient)
		}
		// connect.Error responses must be JSON-encoded.
		if responseContentType == connectUnaryContentTypePrefix+connect.CodecNameJSON ||
			responseContentType == connectUnaryContentTypePrefix+codecNameJSONCharsetUTF8 {
			return nil
		}
		return connect.NewError(
			httpToCode(statusCode),
			statusMsg,
		)
	}
	// Normal responses must have valid content-type that indicates same codec as the request.
	if !strings.HasPrefix(responseContentType, connectUnaryContentTypePrefix) {
		// Doesn't even look like a Connect response? Use code "unknown".
		return connect.Errorf(connect.CodeUnknown,
			"invalid content-type: %q; expecting %q",
			responseContentType,
			connectUnaryContentTypePrefix+requestCodecName,
		)
	}
	responseCodecName := connectCodecForContentType(
		connect.StreamTypeUnary,
		responseContentType,
	)
	if responseCodecName == requestCodecName {
		return nil
	}
	// HACK: We likely want a better way to handle the optional "charset" parameter
	//       for application/json, instead of hard-coding. But this suffices for now.
	if (responseCodecName == connect.CodecNameJSON && requestCodecName == codecNameJSONCharsetUTF8) ||
		(responseCodecName == codecNameJSONCharsetUTF8 && requestCodecName == connect.CodecNameJSON) {
		// Both are JSON
		return nil
	}
	return connect.Errorf(connect.CodeInternal,
		"invalid content-type: %q; expecting %q",
		responseContentType,
		connectUnaryContentTypePrefix+requestCodecName,
	)
}

func connectValidateStreamResponseContentType(requestCodecName string, streamType connect.StreamType, responseContentType string) *connect.Error {
	// Responses must have valid content-type that indicates same codec as the request.
	if !strings.HasPrefix(responseContentType, connectStreamingContentTypePrefix) {
		// Doesn't even look like a Connect response? Use code "unknown".
		return connect.Errorf(
			connect.CodeUnknown,
			"invalid content-type: %q; expecting %q",
			responseContentType,
			connectStreamingContentTypePrefix+requestCodecName,
		)
	}
	responseCodecName := connectCodecForContentType(
		streamType,
		responseContentType,
	)
	if responseCodecName != requestCodecName {
		return connect.Errorf(
			connect.CodeInternal,
			"invalid content-type: %q; expecting %q",
			responseContentType,
			connectStreamingContentTypePrefix+requestCodecName,
		)
	}
	return nil
}

func connectCheckProtocolVersion(request *http.Request, required bool) *connect.Error {
	switch request.Method {
	case http.MethodGet:
		version := request.URL.Query().Get(connectUnaryConnectQueryParameter)
		if version == "" && required {
			return connect.Errorf(connect.CodeInvalidArgument, "missing required query parameter: set %s to %q", connectUnaryConnectQueryParameter, connectUnaryConnectQueryValue)
		} else if version != "" && version != connectUnaryConnectQueryValue {
			return connect.Errorf(connect.CodeInvalidArgument, "%s must be %q: got %q", connectUnaryConnectQueryParameter, connectUnaryConnectQueryValue, version)
		}
	case http.MethodPost:
		version := getHeaderCanonical(request.Header, connectHeaderProtocolVersion)
		if version == "" && required {
			return connect.Errorf(connect.CodeInvalidArgument, "missing required header: set %s to %q", connectHeaderProtocolVersion, connectProtocolVersion)
		} else if version != "" && version != connectProtocolVersion {
			return connect.Errorf(connect.CodeInvalidArgument, "%s must be %q: got %q", connectHeaderProtocolVersion, connectProtocolVersion, version)
		}
	default:
		return connect.Errorf(connect.CodeInvalidArgument, "unsupported method: %q", request.Method)
	}
	return nil
}
