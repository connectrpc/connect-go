// Copyright 2021-2022 Buf Technologies, Inc.
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
	"math"
	"net/http"
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

	connectFlagEnvelopeEndStream = 0b00000010

	connectUnaryContentTypePrefix     = "application/"
	connectUnaryContentTypeJSON       = connectUnaryContentTypePrefix + "json"
	connectStreamingContentTypePrefix = "application/connect+"
)

type protocolConnect struct{}

// NewHandler implements protocol, so it must return an interface.
func (*protocolConnect) NewHandler(params *protocolHandlerParams) protocolHandler {
	contentTypes := make(map[string]struct{})
	for _, name := range params.Codecs.Names() {
		if params.Spec.StreamType == StreamTypeUnary {
			contentTypes[connectUnaryContentTypePrefix+name] = struct{}{}
			continue
		}
		contentTypes[connectStreamingContentTypePrefix+name] = struct{}{}
	}
	return &connectHandler{
		protocolHandlerParams: *params,
		accept:                contentTypes,
	}
}

// NewClient implements protocol, so it must return an interface.
func (*protocolConnect) NewClient(params *protocolClientParams) (protocolClient, error) {
	if err := validateRequestURL(params.URL); err != nil {
		return nil, err
	}
	return &connectClient{protocolClientParams: *params}, nil
}

type connectHandler struct {
	protocolHandlerParams

	accept map[string]struct{}
}

func (h *connectHandler) ContentTypes() map[string]struct{} {
	return h.accept
}

func (*connectHandler) SetTimeout(request *http.Request) (context.Context, context.CancelFunc, error) {
	timeout := request.Header.Get(connectHeaderTimeout)
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

func (h *connectHandler) NewConn(
	responseWriter http.ResponseWriter,
	request *http.Request,
) (handlerConnCloser, bool) {
	// We need to parse metadata before entering the interceptor stack; we'll
	// send the error to the client later on.
	var contentEncoding, acceptEncoding string
	if h.Spec.StreamType == StreamTypeUnary {
		contentEncoding = request.Header.Get(connectUnaryHeaderCompression)
		acceptEncoding = request.Header.Get(connectUnaryHeaderAcceptCompression)
	} else {
		contentEncoding = request.Header.Get(connectStreamingHeaderCompression)
		acceptEncoding = request.Header.Get(connectStreamingHeaderAcceptCompression)
	}
	requestCompression, responseCompression, failed := negotiateCompression(
		h.CompressionPools,
		contentEncoding,
		acceptEncoding,
	)
	if failed == nil {
		failed = checkServerStreamsCanFlush(h.Spec, responseWriter)
	}
	if failed == nil {
		version := request.Header.Get(connectHeaderProtocolVersion)
		if version == "" && h.RequireConnectProtocolHeader {
			failed = errorf(CodeInvalidArgument, "missing required header: set %s to %q", connectHeaderProtocolVersion, connectProtocolVersion)
		} else if version != "" && version != connectProtocolVersion {
			failed = errorf(CodeInvalidArgument, "%s must be %q: got %q", connectHeaderProtocolVersion, connectProtocolVersion, version)
		}
	}

	// Write any remaining headers here:
	// (1) any writes to the stream will implicitly send the headers, so we
	// should get all of gRPC's required response headers ready.
	// (2) interceptors should be able to see these headers.
	//
	// Since we know that these header keys are already in canonical form, we can
	// skip the normalization in Header.Set.
	header := responseWriter.Header()
	header[headerContentType] = []string{request.Header.Get(headerContentType)}
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

	codecName := connectCodecFromContentType(
		h.Spec.StreamType,
		request.Header.Get(headerContentType),
	)
	codec := h.Codecs.Get(codecName) // handler.go guarantees this is not nil

	var conn handlerConnCloser
	peer := Peer{
		Addr:     request.RemoteAddr,
		Protocol: ProtocolConnect,
	}
	if h.Spec.StreamType == StreamTypeUnary {
		conn = &connectUnaryHandlerConn{
			spec:           h.Spec,
			peer:           peer,
			request:        request,
			responseWriter: responseWriter,
			marshaler: connectUnaryMarshaler{
				writer:           responseWriter,
				codec:            codec,
				compressMinBytes: h.CompressMinBytes,
				compressionName:  responseCompression,
				compressionPool:  h.CompressionPools.Get(responseCompression),
				bufferPool:       h.BufferPool,
				header:           responseWriter.Header(),
				sendMaxBytes:     h.SendMaxBytes,
			},
			unmarshaler: connectUnaryUnmarshaler{
				reader:          request.Body,
				codec:           codec,
				compressionPool: h.CompressionPools.Get(requestCompression),
				bufferPool:      h.BufferPool,
				readMaxBytes:    h.ReadMaxBytes,
			},
			responseTrailer: make(http.Header),
		}
	} else {
		conn = &connectStreamingHandlerConn{
			spec:           h.Spec,
			peer:           peer,
			request:        request,
			responseWriter: responseWriter,
			marshaler: connectStreamingMarshaler{
				envelopeWriter: envelopeWriter{
					writer:           responseWriter,
					codec:            codec,
					compressMinBytes: h.CompressMinBytes,
					compressionPool:  h.CompressionPools.Get(responseCompression),
					bufferPool:       h.BufferPool,
					sendMaxBytes:     h.SendMaxBytes,
				},
			},
			unmarshaler: connectStreamingUnmarshaler{
				envelopeReader: envelopeReader{
					reader:          request.Body,
					codec:           codec,
					compressionPool: h.CompressionPools.Get(requestCompression),
					bufferPool:      h.BufferPool,
					readMaxBytes:    h.ReadMaxBytes,
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
}

func (c *connectClient) Peer() Peer {
	return newPeerFromURL(c.URL, ProtocolConnect)
}

func (c *connectClient) WriteRequestHeader(streamType StreamType, header http.Header) {
	// We know these header keys are in canonical form, so we can bypass all the
	// checks in Header.Set.
	if header.Get(headerUserAgent) == "" {
		header[headerUserAgent] = []string{connectUserAgent()}
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
) StreamingClientConn {
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
	var conn StreamingClientConn
	if spec.StreamType == StreamTypeUnary {
		unaryConn := &connectUnaryClientConn{
			spec:             spec,
			peer:             c.Peer(),
			duplexCall:       duplexCall,
			compressionPools: c.CompressionPools,
			bufferPool:       c.BufferPool,
			marshaler: connectUnaryMarshaler{
				writer:           duplexCall,
				codec:            c.Codec,
				compressMinBytes: c.CompressMinBytes,
				compressionName:  c.CompressionName,
				compressionPool:  c.CompressionPools.Get(c.CompressionName),
				bufferPool:       c.BufferPool,
				header:           duplexCall.Header(),
				sendMaxBytes:     c.SendMaxBytes,
			},
			unmarshaler: connectUnaryUnmarshaler{
				reader:       duplexCall,
				codec:        c.Codec,
				bufferPool:   c.BufferPool,
				readMaxBytes: c.ReadMaxBytes,
			},
			responseHeader:  make(http.Header),
			responseTrailer: make(http.Header),
		}
		conn = unaryConn
		duplexCall.SetValidateResponse(unaryConn.validateResponse)
	} else {
		streamingConn := &connectStreamingClientConn{
			spec:             spec,
			peer:             c.Peer(),
			duplexCall:       duplexCall,
			compressionPools: c.CompressionPools,
			bufferPool:       c.BufferPool,
			codec:            c.Codec,
			marshaler: connectStreamingMarshaler{
				envelopeWriter: envelopeWriter{
					writer:           duplexCall,
					codec:            c.Codec,
					compressMinBytes: c.CompressMinBytes,
					compressionPool:  c.CompressionPools.Get(c.CompressionName),
					bufferPool:       c.BufferPool,
					sendMaxBytes:     c.SendMaxBytes,
				},
			},
			unmarshaler: connectStreamingUnmarshaler{
				envelopeReader: envelopeReader{
					reader:       duplexCall,
					codec:        c.Codec,
					bufferPool:   c.BufferPool,
					readMaxBytes: c.ReadMaxBytes,
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
	spec             Spec
	peer             Peer
	duplexCall       *duplexHTTPCall
	compressionPools readOnlyCompressionPools
	bufferPool       *bufferPool
	marshaler        connectUnaryMarshaler
	unmarshaler      connectUnaryUnmarshaler
	responseHeader   http.Header
	responseTrailer  http.Header
}

func (cc *connectUnaryClientConn) Spec() Spec {
	return cc.spec
}

func (cc *connectUnaryClientConn) Peer() Peer {
	return cc.peer
}

func (cc *connectUnaryClientConn) Send(msg any) error {
	if err := cc.marshaler.Marshal(msg); err != nil {
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
	cc.duplexCall.BlockUntilResponseReady()
	if err := cc.unmarshaler.Unmarshal(msg); err != nil {
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

func (cc *connectUnaryClientConn) validateResponse(response *http.Response) *Error {
	for k, v := range response.Header {
		if !strings.HasPrefix(k, connectUnaryTrailerPrefix) {
			cc.responseHeader[k] = v
			continue
		}
		cc.responseTrailer[strings.TrimPrefix(k, connectUnaryTrailerPrefix)] = v
	}
	compression := response.Header.Get(connectUnaryHeaderCompression)
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
	if response.StatusCode != http.StatusOK {
		unmarshaler := connectUnaryUnmarshaler{
			reader:          response.Body,
			compressionPool: cc.compressionPools.Get(compression),
			bufferPool:      cc.bufferPool,
		}
		var wireErr connectWireError
		if err := unmarshaler.UnmarshalFunc(&wireErr, json.Unmarshal); err != nil {
			return NewError(
				connectHTTPToCode(response.StatusCode),
				errors.New(response.Status),
			)
		}
		serverErr := wireErr.asError()
		if serverErr == nil {
			return nil
		}
		serverErr.meta = cc.responseHeader.Clone()
		mergeHeaders(serverErr.meta, cc.responseTrailer)
		return serverErr
	}
	cc.unmarshaler.compressionPool = cc.compressionPools.Get(compression)
	return nil
}

type connectStreamingClientConn struct {
	spec             Spec
	peer             Peer
	duplexCall       *duplexHTTPCall
	compressionPools readOnlyCompressionPools
	bufferPool       *bufferPool
	codec            Codec
	marshaler        connectStreamingMarshaler
	unmarshaler      connectStreamingUnmarshaler
	responseHeader   http.Header
	responseTrailer  http.Header
}

func (cc *connectStreamingClientConn) Spec() Spec {
	return cc.spec
}

func (cc *connectStreamingClientConn) Peer() Peer {
	return cc.peer
}

func (cc *connectStreamingClientConn) Send(msg any) error {
	if err := cc.marshaler.Marshal(msg); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *Error is a non-nil error
}

func (cc *connectStreamingClientConn) RequestHeader() http.Header {
	return cc.duplexCall.Header()
}

func (cc *connectStreamingClientConn) CloseRequest() error {
	return cc.duplexCall.CloseWrite()
}

func (cc *connectStreamingClientConn) Receive(msg any) error {
	cc.duplexCall.BlockUntilResponseReady()
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
		serverErr.meta = cc.responseHeader.Clone()
		mergeHeaders(serverErr.meta, cc.responseTrailer)
		cc.duplexCall.SetError(serverErr)
		return serverErr
	}
	// There's no error in the trailers, so this was probably an error
	// converting the bytes to a message, an error reading from the network, or
	// just an EOF. We're going to return it to the user, but we also want to
	// setResponseError so Send errors out.
	cc.duplexCall.SetError(err)
	return err
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

func (cc *connectStreamingClientConn) validateResponse(response *http.Response) *Error {
	if response.StatusCode != http.StatusOK {
		return errorf(connectHTTPToCode(response.StatusCode), "HTTP status %v", response.Status)
	}
	compression := response.Header.Get(connectStreamingHeaderCompression)
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
	cc.unmarshaler.compressionPool = cc.compressionPools.Get(compression)
	mergeHeaders(cc.responseHeader, response.Header)
	return nil
}

type connectUnaryHandlerConn struct {
	spec            Spec
	peer            Peer
	request         *http.Request
	responseWriter  http.ResponseWriter
	marshaler       connectUnaryMarshaler
	unmarshaler     connectUnaryUnmarshaler
	responseTrailer http.Header
	wroteBody       bool
}

func (hc *connectUnaryHandlerConn) Spec() Spec {
	return hc.spec
}

func (hc *connectUnaryHandlerConn) Peer() Peer {
	return hc.peer
}

func (hc *connectUnaryHandlerConn) Receive(msg any) error {
	if err := hc.unmarshaler.Unmarshal(msg); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *Error is a non-nil error
}

func (hc *connectUnaryHandlerConn) RequestHeader() http.Header {
	return hc.request.Header
}

func (hc *connectUnaryHandlerConn) Send(msg any) error {
	hc.wroteBody = true
	hc.writeResponseHeader(nil /* error */)
	if err := hc.marshaler.Marshal(msg); err != nil {
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
	}
	if err == nil {
		return hc.request.Body.Close()
	}
	// In unary Connect, errors always use application/json.
	hc.responseWriter.Header().Set(headerContentType, connectUnaryContentTypeJSON)
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

func (hc *connectUnaryHandlerConn) writeResponseHeader(err error) {
	header := hc.responseWriter.Header()
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
	spec            Spec
	peer            Peer
	request         *http.Request
	responseWriter  http.ResponseWriter
	marshaler       connectStreamingMarshaler
	unmarshaler     connectStreamingUnmarshaler
	responseTrailer http.Header
}

func (hc *connectStreamingHandlerConn) Spec() Spec {
	return hc.spec
}

func (hc *connectStreamingHandlerConn) Peer() Peer {
	return hc.peer
}

func (hc *connectStreamingHandlerConn) Receive(msg any) error {
	if err := hc.unmarshaler.Unmarshal(msg); err != nil {
		// Clients may not send end-of-stream metadata, so we don't need to handle
		// errSpecialEnvelope.
		return err
	}
	return nil // must be a literal nil: nil *Error is a non-nil error
}

func (hc *connectStreamingHandlerConn) RequestHeader() http.Header {
	return hc.request.Header
}

func (hc *connectStreamingHandlerConn) Send(msg any) error {
	defer flushResponseWriter(hc.responseWriter)
	if err := hc.marshaler.Marshal(msg); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *Error is a non-nil error
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
		return NewError(CodeUnknown, err)
	}
	return nil // must be a literal nil: nil *Error is a non-nil error
}

type connectStreamingMarshaler struct {
	envelopeWriter
}

func (m *connectStreamingMarshaler) MarshalEndStream(err error, trailer http.Header) *Error {
	end := &connectEndStreamMessage{Trailer: trailer}
	if err != nil {
		end.Error = newConnectWireError(err)
		if connectErr, ok := asError(err); ok {
			mergeHeaders(end.Trailer, connectErr.meta)
		}
	}
	data, marshalErr := json.Marshal(end)
	if marshalErr != nil {
		return errorf(CodeInternal, "marshal end stream: %w", marshalErr)
	}
	raw := bytes.NewBuffer(data)
	defer m.envelopeWriter.bufferPool.Put(raw)
	return m.Write(&envelope{
		Data:  raw,
		Flags: connectFlagEnvelopeEndStream,
	})
}

type connectStreamingUnmarshaler struct {
	envelopeReader

	endStreamErr *Error
	trailer      http.Header
}

func (u *connectStreamingUnmarshaler) Unmarshal(message any) *Error {
	err := u.envelopeReader.Unmarshal(message)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errSpecialEnvelope) {
		return err
	}
	env := u.envelopeReader.last
	if !env.IsSet(connectFlagEnvelopeEndStream) {
		return errorf(CodeInternal, "protocol error: invalid envelope flags %d", env.Flags)
	}
	var end connectEndStreamMessage
	if err := json.Unmarshal(env.Data.Bytes(), &end); err != nil {
		return errorf(CodeInternal, "unmarshal end stream message: %w", err)
	}
	u.trailer = end.Trailer
	u.endStreamErr = end.Error.asError()
	return errSpecialEnvelope
}

func (u *connectStreamingUnmarshaler) Trailer() http.Header {
	return u.trailer
}

func (u *connectStreamingUnmarshaler) EndStreamError() *Error {
	return u.endStreamErr
}

type connectUnaryMarshaler struct {
	writer           io.Writer
	codec            Codec
	compressMinBytes int
	compressionName  string
	compressionPool  *compressionPool
	bufferPool       *bufferPool
	header           http.Header
	sendMaxBytes     int
}

func (m *connectUnaryMarshaler) Marshal(message any) *Error {
	if message == nil {
		return m.write(nil)
	}
	data, err := m.codec.Marshal(message)
	if err != nil {
		return errorf(CodeInternal, "marshal message: %w", err)
	}
	// Can't avoid allocating the slice, but we can reuse it.
	uncompressed := bytes.NewBuffer(data)
	defer m.bufferPool.Put(uncompressed)
	if len(data) < m.compressMinBytes || m.compressionPool == nil {
		if m.sendMaxBytes > 0 && len(data) > m.sendMaxBytes {
			return NewError(CodeResourceExhausted, fmt.Errorf("message size %d exceeds sendMaxBytes %d", len(data), m.sendMaxBytes))
		}
		return m.write(data)
	}
	compressed := m.bufferPool.Get()
	defer m.bufferPool.Put(compressed)
	if err := m.compressionPool.Compress(compressed, uncompressed); err != nil {
		return err
	}
	if m.sendMaxBytes > 0 && compressed.Len() > m.sendMaxBytes {
		return NewError(CodeResourceExhausted, fmt.Errorf("compressed message size %d exceeds sendMaxBytes %d", compressed.Len(), m.sendMaxBytes))
	}
	m.header.Set(connectUnaryHeaderCompression, m.compressionName)
	return m.write(compressed.Bytes())
}

func (m *connectUnaryMarshaler) write(data []byte) *Error {
	if _, err := m.writer.Write(data); err != nil {
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		return errorf(CodeUnknown, "write message: %w", err)
	}
	return nil
}

type connectUnaryUnmarshaler struct {
	reader          io.Reader
	codec           Codec
	compressionPool *compressionPool
	bufferPool      *bufferPool
	alreadyRead     bool
	readMaxBytes    int
}

func (u *connectUnaryUnmarshaler) Unmarshal(message any) *Error {
	return u.UnmarshalFunc(message, u.codec.Unmarshal)
}

func (u *connectUnaryUnmarshaler) UnmarshalFunc(message any, unmarshal func([]byte, any) error) *Error {
	if u.alreadyRead {
		return NewError(CodeInternal, io.EOF)
	}
	u.alreadyRead = true
	data := u.bufferPool.Get()
	defer u.bufferPool.Put(data)
	reader := u.reader
	if u.readMaxBytes > 0 && int64(u.readMaxBytes) < math.MaxInt64 {
		reader = io.LimitReader(u.reader, int64(u.readMaxBytes)+1)
	}
	// ReadFrom ignores io.EOF, so any error here is real.
	bytesRead, err := data.ReadFrom(reader)
	if err != nil {
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		if readMaxBytesErr := asMaxBytesError(err, "read first %d bytes of message", bytesRead); readMaxBytesErr != nil {
			return readMaxBytesErr
		}
		return errorf(CodeUnknown, "read message: %w", err)
	}
	if u.readMaxBytes > 0 && bytesRead > int64(u.readMaxBytes) {
		// Attempt to read to end in order to allow connection re-use
		discardedBytes, err := io.Copy(io.Discard, u.reader)
		if err != nil {
			return errorf(CodeResourceExhausted, "message is larger than configured max %d - unable to determine message size: %w", u.readMaxBytes, err)
		}
		return errorf(CodeResourceExhausted, "message size %d is larger than configured max %d", bytesRead+discardedBytes, u.readMaxBytes)
	}
	if data.Len() > 0 && u.compressionPool != nil {
		decompressed := u.bufferPool.Get()
		defer u.bufferPool.Put(decompressed)
		if err := u.compressionPool.Decompress(decompressed, data, int64(u.readMaxBytes)); err != nil {
			return err
		}
		data = decompressed
	}
	if err := unmarshal(data.Bytes(), message); err != nil {
		return errorf(CodeInvalidArgument, "unmarshal into %T: %w", message, err)
	}
	return nil
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
	err := NewError(e.Code, errors.New(e.Message))
	err.wireErr = true
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

// connectUserAgent returns a User-Agent string similar to those used in gRPC.
func connectUserAgent() string {
	return fmt.Sprintf("connect-go/%s (%s)", Version, runtime.Version())
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
