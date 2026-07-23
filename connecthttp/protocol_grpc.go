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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/textproto"
	"runtime"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/bufferpool"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	grpcHeaderCompression       = "Grpc-Encoding"
	grpcHeaderAcceptCompression = "Grpc-Accept-Encoding"
	grpcHeaderTimeout           = "Grpc-Timeout"
	grpcHeaderStatus            = "Grpc-Status"
	grpcHeaderMessage           = "Grpc-Message"
	grpcHeaderDetails           = "Grpc-Status-Details-Bin"

	grpcFlagEnvelopeTrailer = 0b10000000

	grpcContentTypeDefault    = "application/grpc"
	grpcWebContentTypeDefault = "application/grpc-web"
	grpcContentTypePrefix     = grpcContentTypeDefault + "+"
	grpcWebContentTypePrefix  = grpcWebContentTypeDefault + "+"

	headerXUserAgent = "X-User-Agent"

	upperhex = "0123456789ABCDEF"

	// Field numbers of the google.rpc.Status message carried in
	// Grpc-Status-Details-Bin. See
	// https://github.com/googleapis/googleapis/blob/master/google/rpc/status.proto.
	grpcStatusFieldCode    = 1 // int32 code
	grpcStatusFieldMessage = 2 // string message
	grpcStatusFieldDetails = 3 // repeated google.protobuf.Any details
)

var (
	errTrailersWithoutGRPCStatus = fmt.Errorf("protocol error: no %s trailer: %w", grpcHeaderStatus, io.ErrUnexpectedEOF)

	// defaultGrpcUserAgent follows
	// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#user-agents:
	//
	//	While the protocol does not require a user-agent to function it is recommended
	//	that clients provide a structured user-agent string that provides a basic
	//	description of the calling library, version & platform to facilitate issue diagnosis
	//	in heterogeneous environments. The following structure is recommended to library developers:
	//
	//	User-Agent → "grpc-" Language ?("-" Variant) "/" Version ?( " ("  *(AdditionalProperty ";") ")" )
	//
	//nolint:gochecknoglobals
	defaultGrpcUserAgent = fmt.Sprintf("grpc-go-connect/%s (%s)", connect.Version, runtime.Version())
	//nolint:gochecknoglobals
	grpcAllowedMethods = map[string]struct{}{
		http.MethodPost: {},
	}
)

type protocolGRPC struct {
	web bool
}

// NewHandler implements protocol, so it must return an interface.
func (g *protocolGRPC) NewHandler(params *protocolHandlerParams) protocolHandler {
	bare, prefix := grpcContentTypeDefault, grpcContentTypePrefix
	if g.web {
		bare, prefix = grpcWebContentTypeDefault, grpcWebContentTypePrefix
	}
	contentTypes := make(map[string]struct{})
	for _, name := range params.Codecs.Names() {
		contentTypes[canonicalizeContentType(prefix+name)] = struct{}{}
	}
	if params.Codecs.Get(connect.CodecNameProto) != nil {
		contentTypes[bare] = struct{}{}
	}
	return &grpcHandler{
		protocolHandlerParams: *params,
		web:                   g.web,
		accept:                contentTypes,
	}
}

// NewClient implements protocol, so it must return an interface.
func (g *protocolGRPC) NewClient(params *protocolClientParams) (protocolClient, error) {
	peer := newPeerForURL(params.URL, connect.ProtocolNameGRPC)
	if g.web {
		peer = newPeerForURL(params.URL, connect.ProtocolNameGRPCWeb)
	}
	return &grpcClient{
		protocolClientParams: *params,
		web:                  g.web,
		peer:                 peer,
	}, nil
}

type grpcHandler struct {
	protocolHandlerParams

	web    bool
	accept map[string]struct{}
}

func (g *grpcHandler) Methods() map[string]struct{} {
	return grpcAllowedMethods
}

func (g *grpcHandler) ContentTypes() map[string]struct{} {
	return g.accept
}

func (*grpcHandler) SetTimeout(request *http.Request) (context.Context, context.CancelFunc, error) {
	timeout, err := grpcParseTimeout(getHeaderCanonical(request.Header, grpcHeaderTimeout))
	if err != nil && !errors.Is(err, errNoTimeout) {
		// Errors here indicate that the client sent an invalid timeout header, so
		// the error text is safe to send back.
		return nil, nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	} else if err != nil {
		// err wraps errNoTimeout, nothing to do.
		return request.Context(), nil, nil //nolint:nilerr
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	return ctx, cancel, nil
}

func (g *grpcHandler) CanHandlePayload(_ *http.Request, contentType string) bool {
	_, ok := g.accept[contentType]
	return ok
}

func (g *grpcHandler) NewConn(
	responseWriter http.ResponseWriter,
	request *http.Request,
	info *connect.CallInfo,
) (handlerConnCloser, bool) {
	ctx := request.Context()
	// We need to parse metadata before entering the interceptor stack; we'll
	// send the error to the client later on.
	requestCompression, responseCompression, failed := negotiateCompression(
		g.CompressionPools,
		getHeaderCanonical(request.Header, grpcHeaderCompression),
		getHeaderCanonical(request.Header, grpcHeaderAcceptCompression),
	)
	if failed == nil {
		failed = checkServerStreamsCanFlush(g.spec, responseWriter)
	}

	// Write any remaining headers here:
	// (1) any writes to the stream will implicitly send the headers, so we
	// should get all of gRPC's required response headers ready.
	// (2) interceptors should be able to see these headers.
	//
	// Since we know that these header keys are already in canonical form, we can
	// skip the normalization in Header.Set.
	header := responseWriter.Header()
	header[headerContentType] = []string{getHeaderCanonical(request.Header, headerContentType)}
	header[grpcHeaderAcceptCompression] = []string{g.CompressionPools.CommaSeparatedNames()}
	if responseCompression != connect.CompressionNameIdentity {
		header[grpcHeaderCompression] = []string{responseCompression}
	}

	codecName := grpcCodecForContentType(g.web, getHeaderCanonical(request.Header, headerContentType))
	codec := g.Codecs.Get(codecName) // handler.go guarantees this is not nil
	protocolName := connect.ProtocolNameGRPC
	if g.web {
		protocolName = connect.ProtocolNameGRPCWeb
	}
	var sendStats, receiveStats *connect.MessageStats
	if info != nil {
		info.Codec = codecName
		info.RequestEncoding = requestCompression
		info.ResponseEncoding = responseCompression
		sendStats, receiveStats = &info.SendStats, &info.ReceiveStats
	}
	conn := wrapHandlerConnWithCodedErrors(&grpcHandlerConn{
		spec: g.spec,
		peer: peer{
			Addr:     request.RemoteAddr,
			Protocol: protocolName,
		},
		web:      g.web,
		protobuf: g.Codecs.Protobuf(), // for errors
		marshaler: grpcMarshaler{
			envelopeWriter: envelopeWriter{
				ctx:              ctx,
				sender:           writeSender{writer: responseWriter},
				compressionPool:  g.CompressionPools.Get(responseCompression),
				codec:            codec,
				compressMinBytes: g.CompressMinBytes,
				sendMaxBytes:     g.SendMaxBytes,
				stats:            sendStats,
			},
		},
		responseWriter:  responseWriter,
		responseHeader:  make(http.Header),
		responseTrailer: make(http.Header),
		request:         request,
		unmarshaler: grpcUnmarshaler{
			envelopeReader: envelopeReader{
				ctx:             ctx,
				reader:          request.Body,
				codec:           codec,
				compressionPool: g.CompressionPools.Get(requestCompression),
				readMaxBytes:    g.ReadMaxBytes,
				stats:           receiveStats,
			},
			web: g.web,
		},
	})
	if failed != nil {
		// Negotiation failed, so we can't establish a stream.
		_ = conn.Close(failed)
		return nil, false
	}
	return conn, true
}

type grpcClient struct {
	protocolClientParams

	web  bool
	peer peer
}

func (g *grpcClient) Peer() peer {
	return g.peer
}

func (g *grpcClient) WriteRequestHeader(_ connect.StreamType, header http.Header) {
	setUserAgentIfAbsent(header, defaultGrpcUserAgent)
	// We know these header keys are in canonical form, so we can bypass all the
	// checks in Header.Set.
	if g.web && getHeaderCanonical(header, headerXUserAgent) == "" {
		// The gRPC-Web pseudo-specification seems to require X-User-Agent rather
		// than User-Agent for all clients, even if they're not browser-based. This
		// is very odd for a backend client, so we'll split the difference and set
		// both.
		header[headerXUserAgent] = []string{defaultGrpcUserAgent}
	}
	header[headerContentType] = []string{grpcContentTypeForCodecName(g.web, g.Codec.Name())}
	// gRPC handles compression on a per-message basis, so we don't want to
	// compress the whole stream. By default, http.Client will ask the server
	// to gzip the stream if we don't set Accept-Encoding.
	header["Accept-Encoding"] = []string{connect.CompressionNameIdentity}
	if g.CompressionName != "" && g.CompressionName != connect.CompressionNameIdentity {
		header[grpcHeaderCompression] = []string{g.CompressionName}
	}
	if acceptCompression := g.CompressionPools.CommaSeparatedNames(); acceptCompression != "" {
		header[grpcHeaderAcceptCompression] = []string{acceptCompression}
	}
	if !g.web {
		// The gRPC-HTTP2 specification requires this - it flushes out proxies that
		// don't support HTTP trailers.
		header["Te"] = []string{"trailers"}
	}
}

func (g *grpcClient) NewConn(
	ctx context.Context,
	spec connect.Spec,
	header http.Header,
) streamingClientConn {
	if deadline, ok := ctx.Deadline(); ok {
		encodedDeadline := grpcEncodeTimeout(time.Until(deadline))
		header[grpcHeaderTimeout] = []string{encodedDeadline}
	}
	duplexCall := newDuplexHTTPCall(
		ctx,
		g.HTTPClient,
		g.URL,
		spec.StreamType,
		header,
	)
	info, ok := connect.CallInfoForClientContext(ctx)
	var sendStats, receiveStats *connect.MessageStats
	if ok {
		sendStats, receiveStats = &info.SendStats, &info.ReceiveStats
	}
	conn := &grpcClientConn{
		spec:             spec,
		peer:             g.Peer(),
		info:             info,
		duplexCall:       duplexCall,
		compressionPools: g.CompressionPools,
		protobuf:         g.Protobuf,
		marshaler: grpcMarshaler{
			envelopeWriter: envelopeWriter{
				ctx:              ctx,
				sender:           duplexCall,
				compressionPool:  g.CompressionPools.Get(g.CompressionName),
				codec:            g.Codec,
				compressMinBytes: g.CompressMinBytes,
				sendMaxBytes:     g.SendMaxBytes,
				stats:            sendStats,
			},
		},
		unmarshaler: grpcUnmarshaler{
			envelopeReader: envelopeReader{
				ctx:          ctx,
				reader:       duplexCall,
				codec:        g.Codec,
				readMaxBytes: g.ReadMaxBytes,
				stats:        receiveStats,
			},
		},
		responseHeader:  make(http.Header),
		responseTrailer: make(http.Header),
	}
	duplexCall.SetValidateResponse(conn.validateResponse)
	if g.web {
		conn.unmarshaler.web = true
		conn.readTrailers = func(unmarshaler *grpcUnmarshaler, _ *duplexHTTPCall) http.Header {
			return unmarshaler.WebTrailer()
		}
	} else {
		conn.readTrailers = func(_ *grpcUnmarshaler, call *duplexHTTPCall) http.Header {
			// To access HTTP trailers, we need to read the body to EOF.
			_, _ = discard(call)
			return call.ResponseTrailer()
		}
	}
	return wrapClientConnWithCodedErrors(conn)
}

// grpcClientConn works for both gRPC and gRPC-Web.
type grpcClientConn struct {
	spec             connect.Spec
	peer             peer
	info             *connect.CallInfo
	duplexCall       *duplexHTTPCall
	compressionPools readOnlyCompressionPools
	protobuf         connect.Codec // for errors
	marshaler        grpcMarshaler
	unmarshaler      grpcUnmarshaler
	responseHeader   http.Header
	responseTrailer  http.Header
	readTrailers     func(*grpcUnmarshaler, *duplexHTTPCall) http.Header
}

func (cc *grpcClientConn) Spec() connect.Spec {
	return cc.spec
}

func (cc *grpcClientConn) Peer() peer {
	return cc.peer
}

func (cc *grpcClientConn) Send(msg any) error {
	if err := cc.marshaler.Marshal(msg); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *connect.Error is a non-nil error
}

func (cc *grpcClientConn) RequestHeader() http.Header {
	return cc.duplexCall.Header()
}

func (cc *grpcClientConn) CloseRequest() error {
	return cc.duplexCall.CloseWrite()
}

func (cc *grpcClientConn) Receive(msg any) error {
	if _, err := cc.duplexCall.blockUntilResponseReady(); err != nil {
		return err
	}
	err := cc.unmarshaler.Unmarshal(msg)
	if err == nil {
		return nil
	}
	mergeHeaders(
		cc.responseTrailer,
		cc.readTrailers(&cc.unmarshaler, cc.duplexCall),
	)
	if errors.Is(err, io.EOF) && cc.unmarshaler.bytesRead == 0 && len(cc.responseTrailer) == 0 {
		// No body and no trailers means a trailers-only response.
		// Note: per the specification, only the HTTP status code and Content-Type
		// should be treated as headers. The rest should be treated as trailing
		// metadata. But it would be unsafe to mutate cc.responseHeader at this
		// point. So we'll leave cc.responseHeader alone but copy the relevant
		// metadata into cc.responseTrailer.
		mergeHeaders(cc.responseTrailer, cc.responseHeader)
		delHeaderCanonical(cc.responseTrailer, headerContentType)

		// Try to read the status out of the headers.
		serverErr := grpcErrorForTrailer(cc.unmarshaler.ctx, cc.protobuf, cc.responseHeader)
		if serverErr == nil {
			// Status says "OK". So return original error (io.EOF).
			return err
		}
		return serverErr
	}

	// See if the server sent an explicit error in the HTTP or gRPC-Web trailers.
	serverErr := grpcErrorForTrailer(cc.unmarshaler.ctx, cc.protobuf, cc.responseTrailer)
	if serverErr != nil && (errors.Is(err, io.EOF) || !errors.Is(serverErr, errTrailersWithoutGRPCStatus)) {
		// We've either:
		//   - Cleanly read until the end of the response body and *not* received
		//   gRPC status trailers, which is a protocol error, or
		//   - Received an explicit error from the server.
		//
		// This is expected from a protocol perspective, but receiving trailers
		// means that we're _not_ getting a message. For users to realize that
		// the stream has ended, Receive must return an error.
		_ = cc.duplexCall.CloseWrite()
		return serverErr
	}
	// This was probably an error converting the bytes to a message or an error
	// reading from the network. We're going to return it to the
	// user, but we also want to close writes so Send errors out.
	_ = cc.duplexCall.CloseWrite()
	return err
}

func (cc *grpcClientConn) ResponseHeader() http.Header {
	if cc.duplexCall.awaitResponse() {
		return cc.responseHeader
	}
	return make(http.Header)
}

func (cc *grpcClientConn) ResponseTrailer() http.Header {
	if cc.duplexCall.awaitResponse() {
		return cc.responseTrailer
	}
	return make(http.Header)
}

func (cc *grpcClientConn) CloseResponse() error {
	return cc.duplexCall.CloseRead()
}

func (cc *grpcClientConn) onRequestSend(fn func(*http.Request)) {
	cc.duplexCall.onRequestSend = fn
}

func (cc *grpcClientConn) onResponseReceive(fn func(*http.Response)) {
	cc.duplexCall.onResponseReceive = fn
}

func (cc *grpcClientConn) validateResponse(response *http.Response) *connect.Error {
	if err := grpcValidateResponse(
		response,
		cc.responseHeader,
		cc.compressionPools,
		cc.unmarshaler.web,
		cc.marshaler.codec.Name(),
	); err != nil {
		return err
	}
	compression := getHeaderCanonical(response.Header, grpcHeaderCompression)
	cc.unmarshaler.compressionPool = cc.compressionPools.Get(compression)
	if cc.info != nil {
		cc.info.ResponseEncoding = encodingOrIdentity(compression)
	}
	return nil
}

type grpcHandlerConn struct {
	spec            connect.Spec
	peer            peer
	web             bool
	protobuf        connect.Codec // for errors
	marshaler       grpcMarshaler
	responseWriter  http.ResponseWriter
	responseHeader  http.Header
	responseTrailer http.Header
	wroteToBody     bool
	request         *http.Request
	unmarshaler     grpcUnmarshaler
}

func (hc *grpcHandlerConn) Spec() connect.Spec {
	return hc.spec
}

func (hc *grpcHandlerConn) Peer() peer {
	return hc.peer
}

func (hc *grpcHandlerConn) Receive(msg any) error {
	if err := hc.unmarshaler.Unmarshal(msg); err != nil {
		return err // already coded
	}
	return nil // must be a literal nil: nil *connect.Error is a non-nil error
}

func (hc *grpcHandlerConn) RequestHeader() http.Header {
	return hc.request.Header
}

func (hc *grpcHandlerConn) Send(msg any) error {
	defer flushResponseWriter(hc.responseWriter)
	if !hc.wroteToBody {
		mergeHeaders(hc.responseWriter.Header(), hc.responseHeader)
		hc.wroteToBody = true
	}
	if err := hc.marshaler.Marshal(msg); err != nil {
		return err
	}
	return nil // must be a literal nil: nil *connect.Error is a non-nil error
}

func (hc *grpcHandlerConn) ResponseHeader() http.Header {
	return hc.responseHeader
}

func (hc *grpcHandlerConn) ResponseTrailer() http.Header {
	return hc.responseTrailer
}

func (hc *grpcHandlerConn) Close(err error) (retErr error) {
	defer func() {
		// We don't want to copy unread portions of the body to /dev/null here: if
		// the client hasn't closed the request body, we'll block until the server
		// timeout kicks in. This could happen because the client is malicious, but
		// a well-intentioned client may just not expect the server to be returning
		// an error for a streaming RPC. Better to accept that we can't always reuse
		// TCP connections.
		closeErr := hc.request.Body.Close()
		if retErr == nil {
			retErr = closeErr
		}
	}()
	defer flushResponseWriter(hc.responseWriter)
	// If we haven't written the headers yet, do so.
	if !hc.wroteToBody {
		mergeHeaders(hc.responseWriter.Header(), hc.responseHeader)
	}
	// gRPC always sends the error's code, message, details, and metadata as
	// trailing metadata. The Connect protocol doesn't do this, so we don't want
	// to mutate the trailers map that the user sees.
	mergedTrailers := make(
		http.Header,
		len(hc.responseTrailer)+2, // always make space for status & message
	)
	mergeHeaders(mergedTrailers, hc.responseTrailer)
	grpcErrorToTrailer(hc.marshaler.ctx, mergedTrailers, hc.protobuf, err)
	if hc.web && !hc.wroteToBody && len(hc.responseHeader) == 0 {
		// We're using gRPC-Web, we haven't yet written to the body, and there are no
		// custom headers. That means we can send a "trailers-only" response and send
		// trailing metadata as HTTP headers (instead of as trailers).
		mergeHeaders(hc.responseWriter.Header(), mergedTrailers)
		return nil
	}
	if hc.web {
		// We're using gRPC-Web and we've already sent the headers, so we write
		// trailing metadata to the HTTP body.
		if err := hc.marshaler.MarshalWebTrailers(mergedTrailers); err != nil {
			return err
		}
		return nil // must be a literal nil: nil *connect.Error is a non-nil error
	}
	// We're using standard gRPC. Even if we haven't written to the body and
	// we're sending a "trailers-only" response, we must send trailing metadata
	// as HTTP trailers. (If we had frame-level control of the HTTP/2 layer, we
	// could send trailers-only responses as a single HEADER frame and no DATA
	// frames, but net/http doesn't expose APIs that low-level.)
	//
	// In net/http's ResponseWriter API, we send HTTP trailers by writing to the
	// headers map with a special prefix. This prefixing is an implementation
	// detail, so we should hide it and _not_ mutate the user-visible headers.
	//
	// Note that this is _very_ finicky and difficult to test with net/http,
	// since correctness depends on low-level framing details. Breaking this
	// logic breaks Envoy's gRPC-Web translation.
	for key, values := range mergedTrailers {
		for _, value := range values {
			// These are potentially user-supplied, so we can't assume they're in
			// canonical form.
			hc.responseWriter.Header().Add(http.TrailerPrefix+key, value)
		}
	}
	return nil
}

type grpcMarshaler struct {
	envelopeWriter
}

func (m *grpcMarshaler) MarshalWebTrailers(trailer http.Header) *connect.Error {
	raw := bufferpool.Get()
	defer bufferpool.Put(raw)
	for key, values := range trailer {
		// Per the Go specification, keys inserted during iteration may be produced
		// later in the iteration or may be skipped. For safety, avoid mutating the
		// map if the key is already lower-cased.
		lower := strings.ToLower(key)
		if key == lower {
			continue
		}
		delete(trailer, key)
		trailer[lower] = values
	}
	if err := trailer.Write(raw); err != nil {
		return connect.Errorf(connect.CodeInternal, "format trailers: %s", err).WithCause(err)
	}
	return m.Write(&envelope{
		Data:  raw,
		Flags: grpcFlagEnvelopeTrailer,
	})
}

type grpcUnmarshaler struct {
	envelopeReader

	web        bool
	webTrailer http.Header
}

func (u *grpcUnmarshaler) Unmarshal(message any) *connect.Error {
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
	if !u.web || !env.IsSet(grpcFlagEnvelopeTrailer) {
		return connect.Errorf(connect.CodeInternal, "protocol error: invalid envelope flags %d", env.Flags)
	}

	// Per the gRPC-Web specification, trailers should be encoded as an HTTP/1
	// headers block _without_ the terminating newline. To make the headers
	// parseable by net/textproto, we need to add the newline.
	if err := data.WriteByte('\n'); err != nil {
		return connect.Errorf(connect.CodeInternal, "unmarshal web trailers: %s", err).WithCause(err)
	}
	bufferedReader := bufio.NewReader(data)
	mimeReader := textproto.NewReader(bufferedReader)
	mimeHeader, mimeErr := mimeReader.ReadMIMEHeader()
	if mimeErr != nil {
		return connect.Errorf(
			connect.CodeInternal,
			"gRPC-Web protocol error: trailers invalid: %s",
			mimeErr,
		).WithCause(mimeErr)
	}
	u.webTrailer = http.Header(mimeHeader)
	return errSpecialEnvelope
}

func (u *grpcUnmarshaler) WebTrailer() http.Header {
	return u.webTrailer
}

func grpcValidateResponse(
	response *http.Response,
	header http.Header,
	availableCompressors readOnlyCompressionPools,
	web bool,
	codecName string,
) *connect.Error {
	if response.StatusCode != http.StatusOK {
		return connect.Errorf(httpToCode(response.StatusCode), "HTTP status %v", response.Status)
	}
	if err := grpcValidateResponseContentType(
		web,
		codecName,
		getHeaderCanonical(response.Header, headerContentType),
	); err != nil {
		return err
	}
	if compression := getHeaderCanonical(response.Header, grpcHeaderCompression); compression != "" &&
		compression != connect.CompressionNameIdentity &&
		!availableCompressors.Contains(compression) {
		// Per https://github.com/grpc/grpc/blob/master/doc/compression.md, we
		// should return connect.CodeInternal and specify acceptable compression(s) (in
		// addition to setting the Grpc-Accept-Encoding header).
		return connect.Errorf(
			connect.CodeInternal,
			"unknown encoding %q: accepted encodings are %v",
			compression,
			availableCompressors.CommaSeparatedNames(),
		)
	}
	// The response is valid, so we should expose the headers.
	mergeHeaders(header, response.Header)
	return nil
}

// The gRPC wire protocol specifies that errors should be serialized using the
// binary Protobuf format, even if the messages in the request/response stream
// use a different codec. Consequently, this function needs a Protobuf codec to
// unmarshal error information in the headers.
//
// A nil error is only returned when a grpc-status key IS present, but it
// indicates a code of zero (no error). If no grpc-status key is present, this
// returns a non-nil *connect.Error that wraps errTrailersWithoutGRPCStatus.
func grpcErrorForTrailer(ctx context.Context, protobuf connect.Codec, trailer http.Header) *connect.Error {
	codeHeader := getHeaderCanonical(trailer, grpcHeaderStatus)
	if codeHeader == "" {
		// If there are no trailers at all, that's an internal error.
		// But if it's an error determining the status code from the
		// trailers, it's unknown.
		code := connect.CodeUnknown
		if len(trailer) == 0 {
			code = connect.CodeInternal
		}
		return connect.Errorf(code, "%s", errTrailersWithoutGRPCStatus).WithCause(errTrailersWithoutGRPCStatus)
	}
	if codeHeader == "0" {
		return nil
	}

	code, err := strconv.ParseUint(codeHeader, 10 /* base */, 32 /* bitsize */)
	if err != nil {
		return connect.Errorf(connect.CodeUnknown, "protocol error: invalid error code %q", codeHeader)
	}
	message, err := grpcPercentDecode(getHeaderCanonical(trailer, grpcHeaderMessage))
	if err != nil {
		return connect.Errorf(connect.CodeInternal, "protocol error: invalid error message %q", message)
	}
	retErr := connect.NewError(connect.Code(code), message).WithRemote()

	detailsBinaryEncoded := getHeaderCanonical(trailer, grpcHeaderDetails)
	if len(detailsBinaryEncoded) > 0 {
		detailsBinary, err := connect.DecodeBinaryHeader(detailsBinaryEncoded)
		if err != nil {
			return connect.Errorf(connect.CodeInternal, "server returned invalid grpc-status-details-bin trailer: %s", err).WithCause(err)
		}
		statusCode, statusMessage, details, err := grpcUnmarshalStatus(ctx, protobuf, detailsBinary)
		if err != nil {
			return connect.Errorf(connect.CodeInternal, "server returned invalid protobuf for error details: %s", err).WithCause(err)
		}
		// Prefer the Protobuf-encoded data to the headers (grpc-go does this too).
		retErr = connect.NewError(connect.Code(statusCode), statusMessage).WithRemote()
		for _, d := range details {
			detail, _ := connectproto.NewErrorDetail(d) // an *anypb.Any never fails
			retErr = retErr.WithDetail(detail)
		}
	}

	return retErr
}

// grpcMarshalStatus encodes a google.rpc.Status message. The message has only
// three fields, so we encode it directly with protowire rather than depend on
// a generated type that would register itself in the global proto registry.
func grpcMarshalStatus(ctx context.Context, protobuf connect.Codec, code uint32, message string, details []*anypb.Any) ([]byte, error) {
	var buf []byte
	if code != 0 {
		buf = protowire.AppendTag(buf, grpcStatusFieldCode, protowire.VarintType)
		buf = protowire.AppendVarint(buf, uint64(code))
	}
	if message != "" {
		buf = protowire.AppendTag(buf, grpcStatusFieldMessage, protowire.BytesType)
		buf = protowire.AppendString(buf, message)
	}
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	for _, detail := range details {
		if err := protobuf.MarshalWrite(ctx, buffer, detail); err != nil {
			return nil, err
		}
		buf = protowire.AppendTag(buf, grpcStatusFieldDetails, protowire.BytesType)
		buf = protowire.AppendBytes(buf, buffer.Bytes())
		buffer.Reset()
	}
	return buf, nil
}

// grpcUnmarshalStatus decodes a google.rpc.Status message, returning its code,
// message, and details. Unknown fields are skipped so the decoder stays
// forward-compatible with senders that add fields.
func grpcUnmarshalStatus(ctx context.Context, protobuf connect.Codec, data []byte) (uint32, string, []*anypb.Any, error) {
	var (
		code    uint32
		message string
		details []*anypb.Any
	)
	for len(data) > 0 {
		num, typ, size := protowire.ConsumeTag(data)
		if size < 0 {
			return 0, "", nil, protowire.ParseError(size)
		}
		data = data[size:]
		switch {
		case num == grpcStatusFieldCode && typ == protowire.VarintType:
			value, size := protowire.ConsumeVarint(data)
			if size < 0 {
				return 0, "", nil, protowire.ParseError(size)
			}
			code = uint32(value) //nolint:gosec // status code is wire-bounded
			data = data[size:]
		case num == grpcStatusFieldMessage && typ == protowire.BytesType:
			value, size := protowire.ConsumeString(data)
			if size < 0 {
				return 0, "", nil, protowire.ParseError(size)
			}
			message = value
			data = data[size:]
		case num == grpcStatusFieldDetails && typ == protowire.BytesType:
			value, size := protowire.ConsumeBytes(data)
			if size < 0 {
				return 0, "", nil, protowire.ParseError(size)
			}
			detail := &anypb.Any{}
			if err := protobuf.UnmarshalRead(ctx, bytes.NewReader(value), detail); err != nil {
				return 0, "", nil, err
			}
			details = append(details, detail)
			data = data[size:]
		default:
			size := protowire.ConsumeFieldValue(num, typ, data)
			if size < 0 {
				return 0, "", nil, protowire.ParseError(size)
			}
			data = data[size:]
		}
	}
	return code, message, details, nil
}

func grpcParseTimeout(timeout string) (time.Duration, error) {
	if timeout == "" {
		return 0, errNoTimeout
	}
	unit, err := grpcTimeoutUnitLookup(timeout[len(timeout)-1])
	if err != nil {
		return 0, err
	}
	num, err := strconv.ParseInt(timeout[:len(timeout)-1], 10 /* base */, 64 /* bitsize */)
	if err != nil || num < 0 {
		return 0, fmt.Errorf("protocol error: invalid timeout %q", timeout)
	}
	if num > 99999999 { // timeout must be ASCII string of at most 8 digits
		return 0, fmt.Errorf("protocol error: timeout %q is too long", timeout)
	}
	const grpcTimeoutMaxHours = math.MaxInt64 / int64(time.Hour) // how many hours fit into a time.Duration?
	if unit == time.Hour && num > grpcTimeoutMaxHours {
		// Timeout is effectively unbounded, so ignore it. The grpc-go
		// implementation does the same thing.
		return 0, errNoTimeout
	}
	return time.Duration(num) * unit, nil
}

func grpcEncodeTimeout(timeout time.Duration) string {
	if timeout <= 0 {
		return "0n"
	}
	// The gRPC protocol limits timeouts to 8 characters (not counting the unit),
	// so timeouts must be strictly less than 1e8 of the appropriate unit.
	const grpcTimeoutMaxValue = 1e8
	var (
		size time.Duration
		unit byte
	)
	switch {
	case timeout < time.Nanosecond*grpcTimeoutMaxValue:
		size, unit = time.Nanosecond, 'n'
	case timeout < time.Microsecond*grpcTimeoutMaxValue:
		size, unit = time.Microsecond, 'u'
	case timeout < time.Millisecond*grpcTimeoutMaxValue:
		size, unit = time.Millisecond, 'm'
	case timeout < time.Second*grpcTimeoutMaxValue:
		size, unit = time.Second, 'S'
	case timeout < time.Minute*grpcTimeoutMaxValue:
		size, unit = time.Minute, 'M'
	default:
		// time.Duration is an int64 number of nanoseconds, so the largest
		// expressible duration is less than 1e8 hours.
		size, unit = time.Hour, 'H'
	}
	buf := make([]byte, 0, 9)
	buf = strconv.AppendInt(buf, int64(timeout/size), 10 /* base */)
	buf = append(buf, unit)
	return string(buf)
}

func grpcTimeoutUnitLookup(unit byte) (time.Duration, error) {
	switch unit {
	case 'n':
		return time.Nanosecond, nil
	case 'u':
		return time.Microsecond, nil
	case 'm':
		return time.Millisecond, nil
	case 'S':
		return time.Second, nil
	case 'M':
		return time.Minute, nil
	case 'H':
		return time.Hour, nil
	default:
		return 0, fmt.Errorf("protocol error: timeout has invalid unit %q", unit)
	}
}

func grpcCodecForContentType(web bool, contentType string) string {
	if (!web && contentType == grpcContentTypeDefault) || (web && contentType == grpcWebContentTypeDefault) {
		// implicitly protobuf
		return connect.CodecNameProto
	}
	prefix := grpcContentTypePrefix
	if web {
		prefix = grpcWebContentTypePrefix
	}
	return strings.TrimPrefix(contentType, prefix)
}

func grpcContentTypeForCodecName(web bool, name string) string {
	if web {
		return grpcWebContentTypePrefix + name
	}
	if name == connect.CodecNameProto {
		// For compatibility with Google Cloud Platform's frontends, prefer an
		// implicit default codec. See
		// https://github.com/connectrpc/connect-go/pull/655#issuecomment-1915754523
		// for details.
		return grpcContentTypeDefault
	}
	return grpcContentTypePrefix + name
}

func grpcErrorToTrailer(ctx context.Context, trailer http.Header, protobuf connect.Codec, err error) {
	if err == nil {
		setHeaderCanonical(trailer, grpcHeaderStatus, "0") // zero is the gRPC OK status
		return
	}
	var (
		code    = uint32(connect.CodeUnknown)
		message = err.Error()
		bin     []byte
	)
	if connectErr, ok := asError(err); ok {
		code = uint32(connectErr.Code())
		message = connectErr.Message()
		if rawDetails := connectErr.Details(); len(rawDetails) > 0 {
			details := make([]*anypb.Any, 0, len(rawDetails))
			for _, detail := range rawDetails {
				details = append(details, connectproto.ErrorDetailToAny(detail))
			}
			var binErr error
			bin, binErr = grpcMarshalStatus(ctx, protobuf, code, message, details)
			if binErr != nil {
				code = uint32(connect.CodeInternal)
				message = fmt.Sprintf("marshal protobuf status: %v", binErr)
				bin = nil
			}
		}
	}
	setHeaderCanonical(trailer, grpcHeaderStatus, strconv.Itoa(int(code)))
	setHeaderCanonical(trailer, grpcHeaderMessage, grpcPercentEncode(message))
	if len(bin) > 0 {
		setHeaderCanonical(trailer, grpcHeaderDetails, connect.EncodeBinaryHeader(bin))
	}
}

// grpcPercentEncode follows RFC 3986 Section 2.1 and the gRPC HTTP/2 spec.
// It's a variant of URL-encoding with fewer reserved characters. It's intended
// to take UTF-8 encoded text and escape non-ASCII bytes so that they're valid
// HTTP/1 headers, while still maximizing readability of the data on the wire.
//
// The grpc-message trailer (used for human-readable error messages) should be
// percent-encoded.
//
// References:
//
//	https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#responses
//	https://datatracker.ietf.org/doc/html/rfc3986#section-2.1
func grpcPercentEncode(msg string) string {
	var hexCount int
	for i := range len(msg) {
		if grpcShouldEscape(msg[i]) {
			hexCount++
		}
	}
	if hexCount == 0 {
		return msg
	}
	// We need to escape some characters, so we'll need to allocate a new string.
	var out strings.Builder
	out.Grow(len(msg) + 2*hexCount)
	for i := range len(msg) {
		switch char := msg[i]; {
		case grpcShouldEscape(char):
			out.WriteByte('%')
			out.WriteByte(upperhex[char>>4])
			out.WriteByte(upperhex[char&15])
		default:
			out.WriteByte(char)
		}
	}
	return out.String()
}

func grpcPercentDecode(input string) (string, error) {
	percentCount := 0
	for i := 0; i < len(input); {
		switch input[i] {
		case '%':
			percentCount++
			if err := validateHex(input[i:]); err != nil {
				return "", err
			}
			i += 3
		default:
			i++
		}
	}
	if percentCount == 0 {
		return input, nil
	}
	// We need to unescape some characters, so we'll need to allocate a new string.
	var out strings.Builder
	out.Grow(len(input) - 2*percentCount)
	for i := 0; i < len(input); i++ {
		switch input[i] {
		case '%':
			out.WriteByte(unhex(input[i+1])<<4 | unhex(input[i+2]))
			i += 2
		default:
			out.WriteByte(input[i])
		}
	}
	return out.String(), nil
}

// Characters that need to be escaped are defined in gRPC's HTTP/2 spec.
// They're different from the generic set defined in RFC 3986.
func grpcShouldEscape(char byte) bool {
	return char < ' ' || char > '~' || char == '%'
}

func unhex(char byte) byte {
	switch {
	case '0' <= char && char <= '9':
		return char - '0'
	case 'a' <= char && char <= 'f':
		return char - 'a' + 10
	case 'A' <= char && char <= 'F':
		return char - 'A' + 10
	}
	return 0
}

func isHex(char byte) bool {
	return ('0' <= char && char <= '9') || ('a' <= char && char <= 'f') || ('A' <= char && char <= 'F')
}

func validateHex(input string) error {
	if len(input) < 3 || input[0] != '%' || !isHex(input[1]) || !isHex(input[2]) {
		if len(input) > 3 {
			input = input[:3]
		}
		return fmt.Errorf("invalid percent-encoded string %q", input)
	}
	return nil
}

func grpcValidateResponseContentType(web bool, requestCodecName string, responseContentType string) *connect.Error {
	// Responses must have valid content-type that indicates same codec as the request.
	bare, prefix := grpcContentTypeDefault, grpcContentTypePrefix
	if web {
		bare, prefix = grpcWebContentTypeDefault, grpcWebContentTypePrefix
	}
	if responseContentType == prefix+requestCodecName ||
		(requestCodecName == connect.CodecNameProto && responseContentType == bare) {
		return nil
	}
	expectedContentType := bare
	if requestCodecName != connect.CodecNameProto {
		expectedContentType = prefix + requestCodecName
	}
	code := connect.CodeInternal
	if responseContentType != bare && !strings.HasPrefix(responseContentType, prefix) {
		// Doesn't even look like a gRPC response? Use code "unknown".
		code = connect.CodeUnknown
	}
	return connect.Errorf(
		code,
		"invalid content-type: %q; expecting %q",
		responseContentType,
		expectedContentType,
	)
}
