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
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"connectrpc.com/connect/v2"
)

const (
	headerContentType     = "Content-Type"
	headerContentEncoding = "Content-Encoding"
	headerContentLength   = "Content-Length"
	headerHost            = "Host"
	headerUserAgent       = "User-Agent"
	headerTrailer         = "Trailer"
	headerDate            = "Date"

	discardLimit = 1024 * 1024 * 4 // 4MiB
)

var errNoTimeout = errors.New("no timeout")

// A Protocol defines the HTTP semantics to use when sending and receiving
// messages. It ties together codecs, compressors, and net/http to produce
// Senders and Receivers.
//
// For example, connect supports the gRPC protocol using this abstraction. Among
// many other things, the protocol implementation is responsible for
// translating timeouts from Go contexts to HTTP and vice versa. For gRPC, it
// converts timeouts to and from strings (for example, 10*time.Second <->
// "10S"), and puts those strings into the "Grpc-Timeout" HTTP header. Other
// protocols might encode durations differently, put them into a different HTTP
// header, or ignore them entirely.
//
// We don't have any short-term plans to export this interface; it's just here
// to separate the protocol-specific portions of connect from the
// protocol-agnostic plumbing.
type protocol interface {
	NewHandler(*protocolHandlerParams) protocolHandler
	NewClient(*protocolClientParams) (protocolClient, error)
}

// HandlerParams are the arguments provided to a Protocol's NewHandler
// method, bundled into a struct to allow backward-compatible argument
// additions. Protocol implementations should take care to use the supplied
// spec rather than constructing their own, since new fields may have been
// added.
type protocolHandlerParams struct {
	spec                         connect.Spec
	Codecs                       readOnlyCodecs
	CompressionPools             readOnlyCompressionPools
	CompressMinBytes             int
	ReadMaxBytes                 int
	SendMaxBytes                 int
	RequireConnectProtocolHeader bool
	IdempotencyLevel             connect.IdempotencyLevel
}

// Handler is the server side of a protocol. HTTP handlers typically support
// multiple protocols, codecs, and compressors.
type protocolHandler interface {
	// Methods is the set of HTTP methods the protocol can handle.
	Methods() map[string]struct{}

	// ContentTypes is the set of HTTP Content-Types that the protocol can
	// handle.
	ContentTypes() map[string]struct{}

	// SetTimeout runs before NewStream. Implementations may inspect the HTTP
	// request, parse any timeout set by the client, and return a modified
	// context and cancellation function.
	//
	// If the client didn't send a timeout, SetTimeout should return the
	// request's context, a nil cancellation function, and a nil error.
	SetTimeout(*http.Request) (context.Context, context.CancelFunc, error)

	// CanHandlePayload returns true if the protocol can handle an HTTP request.
	// This is called after the request method is validated, so we only need to
	// be concerned with the content type/payload specifically.
	CanHandlePayload(*http.Request, string) bool

	// NewConn constructs a HandlerConn for the message exchange. It populates
	// the [connect.CallInfo]'s codec, encoding, and stats fields.
	NewConn(http.ResponseWriter, *http.Request, *connect.CallInfo) (handlerConnCloser, bool)
}

// ClientParams are the arguments provided to a Protocol's NewClient method,
// bundled into a struct to allow backward-compatible argument additions.
// Protocol implementations should take care to use the supplied spec rather
// than constructing their own, since new fields may have been added.
type protocolClientParams struct {
	CompressionName  string
	CompressionPools readOnlyCompressionPools
	Codec            connect.Codec
	CompressMinBytes int
	HTTPClient       HTTPClient
	URL              *url.URL
	ReadMaxBytes     int
	SendMaxBytes     int
	EnableGet        bool
	GetURLMaxBytes   int
	GetUseFallback   bool
	// The gRPC family of protocols always needs access to a Protobuf codec to
	// marshal and unmarshal errors.
	Protobuf connect.Codec
}

// Client is the client side of a protocol. HTTP clients typically use a single
// protocol, codec, and compressor to send requests.
type protocolClient interface {
	// peer describes the server for the RPC.
	Peer() peer

	// WriteRequestHeader writes any protocol-specific request headers.
	WriteRequestHeader(connect.StreamType, http.Header)

	// NewConn constructs a StreamingClientConn for the message exchange.
	//
	// Implementations should assume that the supplied HTTP headers have already
	// been populated by WriteRequestHeader. When constructing a stream for a
	// unary call, implementations may assume that the Sender's Send and Close
	// methods return before the Receiver's Receive or Close methods are called.
	NewConn(context.Context, connect.Spec, http.Header) streamingClientConn
}

// streamingClientConn is the client's view of a bidirectional message exchange,
// plus a method for registering a hook when the HTTP request is actually sent.
type streamingClientConn interface {
	Spec() connect.Spec
	Peer() peer
	Send(any) error
	RequestHeader() http.Header
	CloseRequest() error
	Receive(any) error
	ResponseHeader() http.Header
	ResponseTrailer() http.Header
	CloseResponse() error

	onRequestSend(fn func(*http.Request))
	onResponseReceive(fn func(*http.Response))
}

// errorTranslatingHandlerConnCloser wraps a handlerConnCloser to ensure that
// we always return coded errors to users and write coded errors to the
// network.
//
// It's used in protocol implementations.
type errorTranslatingHandlerConnCloser struct {
	handlerConnCloser

	toWire   func(error) error
	fromWire func(error) error
}

func (hc *errorTranslatingHandlerConnCloser) Send(msg any) error {
	return hc.fromWire(hc.handlerConnCloser.Send(msg))
}

func (hc *errorTranslatingHandlerConnCloser) Receive(msg any) error {
	return hc.fromWire(hc.handlerConnCloser.Receive(msg))
}

func (hc *errorTranslatingHandlerConnCloser) Close(err error) error {
	closeErr := hc.handlerConnCloser.Close(hc.toWire(err))
	return hc.fromWire(closeErr)
}

func (hc *errorTranslatingHandlerConnCloser) getHTTPMethod() string {
	if methoder, ok := hc.handlerConnCloser.(interface{ getHTTPMethod() string }); ok {
		return methoder.getHTTPMethod()
	}
	return http.MethodPost
}

// errorTranslatingClientConn wraps a StreamingClientConn to make sure that we always
// return coded errors from clients.
//
// It's used in protocol implementations.
type errorTranslatingClientConn struct {
	streamingClientConn

	fromWire func(error) error
}

func (cc *errorTranslatingClientConn) Send(msg any) error {
	return cc.fromWire(cc.streamingClientConn.Send(msg))
}

func (cc *errorTranslatingClientConn) Receive(msg any) error {
	return cc.fromWire(cc.streamingClientConn.Receive(msg))
}

func (cc *errorTranslatingClientConn) CloseRequest() error {
	return cc.fromWire(cc.streamingClientConn.CloseRequest())
}

func (cc *errorTranslatingClientConn) CloseResponse() error {
	return cc.fromWire(cc.streamingClientConn.CloseResponse())
}

func (cc *errorTranslatingClientConn) onRequestSend(fn func(*http.Request)) {
	cc.streamingClientConn.onRequestSend(fn)
}

func (cc *errorTranslatingClientConn) onResponseReceive(fn func(*http.Response)) {
	cc.streamingClientConn.onResponseReceive(fn)
}

// wrapHandlerConnWithCodedErrors ensures that we (1) scrub handler errors
// before writing them to the network, and (2) return *Errors from all
// exported APIs.
func wrapHandlerConnWithCodedErrors(conn handlerConnCloser) handlerConnCloser {
	return &errorTranslatingHandlerConnCloser{
		handlerConnCloser: conn,
		toWire:            scrubHandlerError,
		fromWire:          wrapIfUncoded,
	}
}

// wrapClientConnWithCodedErrors ensures that we always return *Errors from
// public APIs.
func wrapClientConnWithCodedErrors(conn streamingClientConn) streamingClientConn {
	return &errorTranslatingClientConn{
		streamingClientConn: conn,
		fromWire:            wrapIfUncoded,
	}
}

func mappedMethodHandlers(handlers []protocolHandler) map[string][]protocolHandler {
	methodHandlers := make(map[string][]protocolHandler)
	for _, handler := range handlers {
		for method := range handler.Methods() {
			methodHandlers[method] = append(methodHandlers[method], handler)
		}
	}
	return methodHandlers
}

func sortedAcceptPostValue(handlers []protocolHandler) string {
	contentTypes := make(map[string]struct{})
	for _, handler := range handlers {
		for contentType := range handler.ContentTypes() {
			contentTypes[contentType] = struct{}{}
		}
	}
	accept := make([]string, 0, len(contentTypes))
	for ct := range contentTypes {
		accept = append(accept, ct)
	}
	sort.Strings(accept)
	return strings.Join(accept, ", ")
}

func sortedAllowMethodValue(handlers []protocolHandler) string {
	methods := make(map[string]struct{})
	for _, handler := range handlers {
		for method := range handler.Methods() {
			methods[method] = struct{}{}
		}
	}
	allow := make([]string, 0, len(methods))
	for ct := range methods {
		allow = append(allow, ct)
	}
	sort.Strings(allow)
	return strings.Join(allow, ", ")
}

func isCommaOrSpace(c rune) bool {
	return c == ',' || c == ' '
}

func discard(reader io.Reader) (int64, error) {
	if lr, ok := reader.(*io.LimitedReader); ok {
		return io.Copy(io.Discard, lr)
	}
	// We don't want to get stuck throwing data away forever, so limit how much
	// we're willing to do here.
	lr := &io.LimitedReader{R: reader, N: discardLimit}
	return io.Copy(io.Discard, lr)
}

// negotiateCompression determines and validates the request compression and
// response compression using the available compressors and protocol-specific
// Content-Encoding and Accept-Encoding headers.
func negotiateCompression( //nolint:nonamedreturns
	availableCompressors readOnlyCompressionPools,
	sent, accept string,
) (requestCompression, responseCompression string, clientVisibleErr *connect.Error) {
	requestCompression = connect.CompressionNameIdentity
	if sent != "" && sent != connect.CompressionNameIdentity {
		// We default to identity, so we only care if the client sends something
		// other than the empty string or compressIdentity.
		if availableCompressors.Contains(sent) {
			requestCompression = sent
		} else {
			// To comply with
			// https://github.com/grpc/grpc/blob/master/doc/compression.md and the
			// Connect protocol, we should return connect.CodeUnimplemented and specify
			// acceptable compression(s) (in addition to setting the a
			// protocol-specific accept-encoding header).
			return "", "", connect.Errorf(
				connect.CodeUnimplemented,
				"unknown compression %q: supported encodings are %v",
				sent, availableCompressors.CommaSeparatedNames(),
			)
		}
	}
	// Support asymmetric compression. This logic follows
	// https://github.com/grpc/grpc/blob/master/doc/compression.md and common
	// sense.
	responseCompression = requestCompression
	// If we're not already planning to compress the response, check whether the
	// client requested a compression algorithm we support.
	if responseCompression == connect.CompressionNameIdentity && accept != "" {
		for _, name := range strings.FieldsFunc(accept, isCommaOrSpace) {
			if availableCompressors.Contains(name) {
				// We found a mutually supported compression algorithm. Unlike standard
				// HTTP, there's no preference weighting, so can bail out immediately.
				responseCompression = name
				break
			}
		}
	}
	return requestCompression, responseCompression, nil
}

// checkServerStreamsCanFlush ensures that bidi and server streaming handlers
// have received an http.ResponseWriter that implements http.Flusher, since
// they must flush data after sending each message.
func checkServerStreamsCanFlush(spec connect.Spec, responseWriter http.ResponseWriter) *connect.Error {
	requiresFlusher := (spec.StreamType & connect.StreamTypeServer) == connect.StreamTypeServer
	if _, flushable := responseWriter.(http.Flusher); requiresFlusher && !flushable {
		return connect.Errorf(connect.CodeInternal, "%T does not implement http.Flusher", responseWriter)
	}
	return nil
}

func flushResponseWriter(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func canonicalizeContentType(contentType string) string {
	// Typically, clients send Content-Type in canonical form, without
	// parameters. In those cases, we'd like to avoid parsing and
	// canonicalization overhead.
	//
	// See https://www.rfc-editor.org/rfc/rfc2045.html#section-5.1 for a full
	// grammar.
	var slashes int
	for _, r := range contentType {
		switch {
		case r >= 'a' && r <= 'z':
		case r == '.' || r == '+' || r == '-':
		case r == '/':
			slashes++
		default:
			return canonicalizeContentTypeSlow(contentType)
		}
	}
	if slashes == 1 {
		return contentType
	}
	return canonicalizeContentTypeSlow(contentType)
}

func canonicalizeContentTypeSlow(contentType string) string {
	base, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return contentType
	}
	// According to RFC 9110 Section 8.3.2, the charset parameter value should be treated as case-insensitive.
	// Connect payloads are always UTF-8, so a utf-8 charset is dropped to canonicalize to the bare
	// content type; any other charset is retained (and rejected during codec negotiation).
	// ref.) https://httpwg.org/specs/rfc9110.html#rfc.section.8.3.2
	if charset, ok := params["charset"]; ok {
		if strings.ToLower(charset) == "utf-8" {
			delete(params, "charset")
		} else {
			params["charset"] = strings.ToLower(charset)
		}
	}
	return mime.FormatMediaType(base, params)
}

func httpToCode(httpCode int) connect.Code {
	// https://github.com/grpc/grpc/blob/master/doc/http-grpc-status-mapping.md
	// Note that this is NOT the inverse of the gRPC-to-HTTP or Connect-to-HTTP
	// mappings.

	// Literals are easier to compare to the specification (vs named
	// constants).
	switch httpCode {
	case 400:
		return connect.CodeInternal
	case 401:
		return connect.CodeUnauthenticated
	case 403:
		return connect.CodePermissionDenied
	case 404:
		return connect.CodeUnimplemented
	case 429:
		return connect.CodeUnavailable
	case 502, 503, 504:
		return connect.CodeUnavailable
	default:
		return connect.CodeUnknown
	}
}
