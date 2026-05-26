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

package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Connect-over-WebSocket transport (RFC 008), Phase 2 implementation.
//
// The wire format reuses Connect's existing Enveloped-Message framing: each
// WebSocket binary message carries exactly one envelope (5-byte prefix +
// payload). The transport plugs into connect-go's protocolHandler abstraction
// as a fourth sibling alongside Connect, gRPC, and gRPC-Web — see the design
// note that preceded this file in the Phase 1 checkpoint.
//
// Two new client-only envelope flag bits are introduced by the RFC:
//
//   - Bit 2 (0x04) End-Of-Client-Stream — substitutes for HTTP request-body
//     EOF, which WebSocket cannot half-close. Carried by the client's final
//     envelope; the payload may be empty or the final data message.
//   - Bit 3 (0x08) Leading-Metadata — JSON-encoded http.Header that the
//     browser WebSocket API cannot send as HTTP headers on the upgrade.
//
// Server-to-client framing is unchanged from the existing Connect streaming
// protocol: zero or more data envelopes followed by an EndStream-Envelope
// (bit 1 set, payload is the EndStreamMessage JSON). connectStreamingMarshaler
// is reused verbatim to produce that final envelope.

const (
	// ProtocolConnectWebSocket identifies the WebSocket transport for the
	// Connect protocol on the [Peer].Protocol field.
	ProtocolConnectWebSocket = "connect+ws"

	wsSubprotocolBase  = "connect.v1"
	wsSubprotocolProto = "connect.v1+proto"
	wsSubprotocolJSON  = "connect.v1+json"

	wsHeaderUpgrade    = "Upgrade"
	wsHeaderConnection = "Connection"
	wsHeaderProtocol   = "Sec-WebSocket-Protocol"
	wsHeaderExtensions = "Sec-WebSocket-Extensions"

	wsQueryTimeoutMs = "connect-timeout-ms"

	// Client-only envelope flag bits introduced by RFC 008.
	wsFlagEnvelopeEndClientStream = 0b00000100 // bit 2
	wsFlagEnvelopeLeadingMetadata = 0b00001000 // bit 3

	// wsCloseWriteTimeout bounds how long Close waits to flush the WebSocket
	// close frame after the EndStream envelope has been written. Chosen short
	// because if the peer is gone the OS will tear down the TCP connection
	// anyway.
	wsCloseWriteTimeout = 5 * time.Second
)

type protocolWebSocket struct{}

// NewHandler implements protocol, so it must return an interface.
func (*protocolWebSocket) NewHandler(params *protocolHandlerParams) protocolHandler {
	contentTypes := make(map[string]struct{})
	for _, name := range params.Codecs.Names() {
		contentTypes[canonicalizeContentType(connectStreamingContentTypePrefix+name)] = struct{}{}
	}
	return &websocketHandler{
		protocolHandlerParams: *params,
		accept:                contentTypes,
		upgrader: &websocket.Upgrader{
			// Origin enforcement is the application's responsibility — interceptors
			// run before any envelope is read and can inspect r.Header.
			CheckOrigin: func(*http.Request) bool { return true },
			// CompressionMode is zero, which disables permessage-deflate. RFC 008
			// forbids it; see also the explicit Sec-WebSocket-Extensions check in
			// NewConn below.
		},
	}
}

// NewClient implements protocol, so it must return an interface.
func (*protocolWebSocket) NewClient(params *protocolClientParams) (protocolClient, error) {
	// Translate http(s) → ws(s) for the WebSocket upgrade URL.
	dialURL := *params.URL
	switch strings.ToLower(dialURL.Scheme) {
	case "http", "ws", "":
		dialURL.Scheme = "ws"
	case "https", "wss":
		dialURL.Scheme = "wss"
	default:
		return nil, fmt.Errorf("WebSocket transport: unsupported URL scheme %q", params.URL.Scheme)
	}
	return &websocketClient{
		protocolClientParams: *params,
		dialURL:              &dialURL,
		peer:                 newPeerForURL(params.URL, ProtocolConnectWebSocket),
	}, nil
}

type websocketHandler struct {
	protocolHandlerParams

	accept   map[string]struct{}
	upgrader *websocket.Upgrader
}

func (h *websocketHandler) Methods() map[string]struct{} {
	return map[string]struct{}{http.MethodGet: {}}
}

func (h *websocketHandler) ContentTypes() map[string]struct{} {
	// Only the non-browser Content-Type names appear here; the Accept-Post
	// HTTP header derived from this set is irrelevant for browser clients,
	// which negotiate codecs through the subprotocol token instead.
	return h.accept
}

func (h *websocketHandler) SetTimeout(request *http.Request) (context.Context, context.CancelFunc, error) {
	headerVal := getHeaderCanonical(request.Header, connectHeaderTimeout)
	queryVal := request.URL.Query().Get(wsQueryTimeoutMs)
	var timeout string
	switch {
	case headerVal != "" && queryVal != "" && headerVal != queryVal:
		return nil, nil, errorf(
			CodeInvalidArgument,
			"conflicting %s header (%q) and %s query parameter (%q)",
			connectHeaderTimeout, headerVal, wsQueryTimeoutMs, queryVal,
		)
	case headerVal != "":
		timeout = headerVal
	case queryVal != "":
		timeout = queryVal
	default:
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

func (h *websocketHandler) CanHandlePayload(request *http.Request, _ string) bool {
	if !isWebSocketUpgrade(request) {
		return false
	}
	_, ok := pickConnectSubprotocol(request)
	return ok
}

func (h *websocketHandler) NewConn(
	responseWriter http.ResponseWriter,
	request *http.Request,
) (handlerConnCloser, bool) {
	ctx := request.Context()

	// RFC 008 says servers "MUST NOT negotiate permessage-deflate" and that
	// "servers MUST reject upgrades that request it." The second clause is
	// impractical because browsers offer permessage-deflate on every upgrade
	// — there is no Web API to suppress it — so a literal-MUST-reject server
	// cannot accept any browser client. The practical interpretation, and
	// what this implementation does, is to *decline* the extension without
	// failing the handshake: gorilla.Upgrader.CompressionMode defaults to
	// zero, so the response omits Sec-WebSocket-Extensions and no extension
	// is in effect for the connection. This relaxation should be folded back
	// into the RFC.
	subprotocol, ok := pickConnectSubprotocol(request)
	if !ok {
		// CanHandlePayload should have rejected the request before we got
		// here, but stay defensive.
		http.Error(
			responseWriter,
			"missing or unsupported Sec-WebSocket-Protocol",
			http.StatusBadRequest,
		)
		return nil, false
	}

	codecName, codecErr := chooseWebSocketCodec(
		subprotocol,
		getHeaderCanonical(request.Header, headerContentType),
	)
	if codecErr != nil {
		http.Error(responseWriter, codecErr.Error(), http.StatusBadRequest)
		return nil, false
	}
	codec := h.Codecs.Get(codecName)
	if codec == nil {
		http.Error(
			responseWriter,
			fmt.Sprintf("unsupported message encoding: %q", codecName),
			http.StatusUnsupportedMediaType,
		)
		return nil, false
	}

	// Compression negotiation. Browsers cannot set Connect-Content-Encoding;
	// the query-parameter fallback path is left for a follow-up.
	requestCompression, responseCompression, failed := negotiateCompression(
		h.CompressionPools,
		getHeaderCanonical(request.Header, connectStreamingHeaderCompression),
		getHeaderCanonical(request.Header, connectStreamingHeaderAcceptCompression),
	)

	// Fail fast with a clear error if the listener does not support hijacking
	// (e.g. h2c-only deployments). gorilla's Upgrade would also fail, but with
	// a less specific message.
	if _, hijackable := responseWriter.(http.Hijacker); !hijackable {
		http.Error(
			responseWriter,
			"Connect over WebSocket requires the HTTP listener to support http.Hijacker; "+
				"see https://connectrpc.com/docs/go/deployment/#h2c for enabling HTTP/1.1",
			http.StatusInternalServerError,
		)
		return nil, false
	}

	// Echo the chosen subprotocol on the upgrade response. We do the picking
	// ourselves rather than relying on Upgrader.Subprotocols so we can honor
	// the client's preference order without mutating the shared Upgrader.
	//
	// Also announce the response compression here: HTTP response headers
	// cannot be added after the 101, so this is the only opportunity to tell
	// the client which compression algorithm to expect on response envelopes.
	upgradeResponseHeader := http.Header{wsHeaderProtocol: []string{subprotocol}}
	if responseCompression != compressionIdentity {
		upgradeResponseHeader.Set(connectStreamingHeaderCompression, responseCompression)
	}
	wsConn, upgradeErr := h.upgrader.Upgrade(responseWriter, request, upgradeResponseHeader)
	if upgradeErr != nil {
		// gorilla has already written an HTTP error response.
		return nil, false
	}

	peer := Peer{
		Addr:     request.RemoteAddr,
		Protocol: ProtocolConnectWebSocket,
		Query:    request.URL.Query(),
	}
	conn := &websocketHandlerConn{
		spec:    h.Spec,
		peer:    peer,
		request: request,
		wsConn:  wsConn,
		marshaler: connectStreamingMarshaler{
			envelopeWriter: envelopeWriter{
				ctx:              ctx,
				sender:           &websocketBinarySender{conn: wsConn},
				codec:            codec,
				compressMinBytes: h.CompressMinBytes,
				compressionPool:  h.CompressionPools.Get(responseCompression),
				bufferPool:       h.BufferPool,
				sendMaxBytes:     h.SendMaxBytes,
			},
		},
		unmarshaler: websocketUnmarshaler{
			envelopeReader: envelopeReader{
				ctx:             ctx,
				codec:           codec,
				compressionPool: h.CompressionPools.Get(requestCompression),
				bufferPool:      h.BufferPool,
				readMaxBytes:    h.ReadMaxBytes,
			},
			wsConn:        wsConn,
			requestHeader: request.Header.Clone(),
		},
		responseHeader:  make(http.Header),
		responseTrailer: make(http.Header),
	}
	wrapped := wrapHandlerConnWithCodedErrors(conn)
	if failed != nil {
		_ = wrapped.Close(failed)
		return nil, false
	}
	return wrapped, true
}

// websocketHandlerConn is the server's view of a Connect-over-WebSocket
// exchange. It satisfies handlerConnCloser and is driven by the same
// implementation function as every other streaming protocol.
type websocketHandlerConn struct {
	spec    Spec
	peer    Peer
	request *http.Request
	wsConn  *websocket.Conn

	marshaler   connectStreamingMarshaler
	unmarshaler websocketUnmarshaler

	responseHeader  http.Header
	responseTrailer http.Header
}

func (c *websocketHandlerConn) Spec() Spec { return c.spec }
func (c *websocketHandlerConn) Peer() Peer { return c.peer }

func (c *websocketHandlerConn) Receive(msg any) error {
	if err := c.unmarshaler.Unmarshal(msg); err != nil {
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

func (c *websocketHandlerConn) RequestHeader() http.Header {
	return c.unmarshaler.requestHeader
}

func (c *websocketHandlerConn) Send(msg any) error {
	if err := c.marshaler.Marshal(msg); err != nil {
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

// ResponseHeader returns the response metadata bag. Because the HTTP response
// is committed at the WebSocket upgrade, these values are folded into the
// EndStreamMessage trailers at Close time rather than sent as HTTP headers.
func (c *websocketHandlerConn) ResponseHeader() http.Header {
	return c.responseHeader
}

func (c *websocketHandlerConn) ResponseTrailer() http.Header {
	return c.responseTrailer
}

func (c *websocketHandlerConn) Close(err error) error {
	// Fold response headers into trailers. Both arrive at the client in the
	// EndStreamMessage.metadata field; the distinction between "header" and
	// "trailer" has no on-the-wire meaning after a WebSocket upgrade.
	if len(c.responseHeader) > 0 {
		mergeHeaders(c.responseTrailer, c.responseHeader)
	}
	marshalErr := c.marshaler.MarshalEndStream(err, c.responseTrailer)
	closeCode := websocket.CloseNormalClosure
	var closeMessage string
	if marshalErr != nil {
		closeCode = websocket.CloseInternalServerErr
		closeMessage = marshalErr.Message()
	}
	// Best-effort flush of the WebSocket close frame. Errors here are not
	// actionable: the TCP connection is about to be closed regardless.
	_ = c.wsConn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(closeCode, closeMessage),
		time.Now().Add(wsCloseWriteTimeout),
	)
	_ = c.wsConn.Close()
	if marshalErr != nil {
		return marshalErr
	}
	return nil
}

// getHTTPMethod reports the HTTP method used to establish the connection. The
// WebSocket transport always uses GET (RFC 6455 §4.1).
func (c *websocketHandlerConn) getHTTPMethod() string {
	return http.MethodGet
}

// websocketBinarySender writes each envelope as a single WebSocket binary
// message. It substitutes for writeSender{responseWriter} in the existing
// protocols: the same envelope bytes go out, but framed as a WebSocket binary
// frame rather than appended to the HTTP response body.
type websocketBinarySender struct {
	conn *websocket.Conn
}

func (s *websocketBinarySender) Send(payload messagePayload) (int64, error) {
	writer, err := s.conn.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return 0, err
	}
	wroteN, writeErr := payload.WriteTo(writer)
	closeErr := writer.Close()
	if writeErr != nil {
		return wroteN, writeErr
	}
	return wroteN, closeErr
}

// websocketUnmarshaler reads one envelope per WebSocket binary frame and
// transparently handles bit 2 (End-Of-Client-Stream) and bit 3
// (Leading-Metadata) before delegating data envelopes to the standard
// envelopeReader machinery. The reader field of the embedded envelopeReader is
// swapped to a fresh per-frame io.Reader on every Unmarshal call.
type websocketUnmarshaler struct {
	envelopeReader

	wsConn        *websocket.Conn
	requestHeader http.Header

	eof bool
}

func (u *websocketUnmarshaler) Unmarshal(message any) *Error {
	if u.eof {
		return NewError(CodeUnknown, io.EOF)
	}
	for {
		messageType, frame, readerErr := u.wsConn.NextReader()
		if readerErr != nil {
			u.eof = true
			if isCleanWebSocketClose(readerErr) {
				return NewError(CodeUnknown, io.EOF)
			}
			// Abrupt close from the client before EndStream — RFC 008 §"Errors"
			// maps this to canceled.
			return errorf(CodeCanceled, "websocket closed before end-of-stream: %w", readerErr)
		}
		if messageType != websocket.BinaryMessage {
			_, _ = io.Copy(io.Discard, frame)
			return errorf(
				CodeInvalidArgument,
				"Connect over WebSocket requires binary frames; got message type %d",
				messageType,
			)
		}

		// Reuse envelopeReader.Read against this single-frame reader. The
		// reader field is mutated here; the embedded reader is otherwise
		// stateful only via bytesRead, which we reset.
		u.envelopeReader.reader = frame
		u.envelopeReader.bytesRead = 0
		buffer := u.envelopeReader.bufferPool.Get()
		env := &envelope{Data: buffer}
		if readErr := u.envelopeReader.Read(env); readErr != nil {
			u.envelopeReader.bufferPool.Put(buffer)
			return readErr
		}
		// Each binary frame must contain exactly one envelope.
		if extra, _ := io.Copy(io.Discard, frame); extra > 0 {
			u.envelopeReader.bufferPool.Put(buffer)
			return errorf(
				CodeInvalidArgument,
				"websocket frame contains %d extra bytes after envelope",
				extra,
			)
		}

		flags := env.Flags
		switch {
		case flags&wsFlagEnvelopeLeadingMetadata != 0:
			mergeErr := u.mergeLeadingMetadata(env)
			u.envelopeReader.bufferPool.Put(buffer)
			if mergeErr != nil {
				return mergeErr
			}
			continue
		case flags&wsFlagEnvelopeEndClientStream != 0:
			// Final envelope from the client. If it carries a payload, deliver
			// it as a normal message; mark EOF either way so the next Receive
			// call returns io.EOF.
			u.eof = true
			if env.Data.Len() == 0 {
				u.envelopeReader.bufferPool.Put(buffer)
				return NewError(CodeUnknown, io.EOF)
			}
			decodeErr := u.decodeData(env, message)
			u.envelopeReader.bufferPool.Put(buffer)
			return decodeErr
		case flags == 0 || flags == flagEnvelopeCompressed:
			decodeErr := u.decodeData(env, message)
			u.envelopeReader.bufferPool.Put(buffer)
			return decodeErr
		default:
			u.envelopeReader.bufferPool.Put(buffer)
			return errorf(
				CodeInvalidArgument,
				"client sent envelope with reserved flags: 0x%02x", flags,
			)
		}
	}
}

func (u *websocketUnmarshaler) decodeData(env *envelope, message any) *Error {
	data := env.Data
	if env.IsSet(flagEnvelopeCompressed) {
		if u.envelopeReader.compressionPool == nil {
			return errorf(
				CodeInternal,
				"protocol error: client sent compressed envelope without compression support",
			)
		}
		decompressed := u.envelopeReader.bufferPool.Get()
		defer u.envelopeReader.bufferPool.Put(decompressed)
		if err := u.envelopeReader.compressionPool.Decompress(
			decompressed, data, int64(u.envelopeReader.readMaxBytes),
		); err != nil {
			return err
		}
		data = decompressed
	}
	if data.Len() == 0 {
		// Zero value of the message is correct.
		return nil
	}
	if err := u.envelopeReader.codec.Unmarshal(data.Bytes(), message); err != nil {
		return errorf(CodeInvalidArgument, "unmarshal message: %w", err)
	}
	return nil
}

func (u *websocketUnmarshaler) mergeLeadingMetadata(env *envelope) *Error {
	data := env.Data
	if env.IsSet(flagEnvelopeCompressed) {
		if u.envelopeReader.compressionPool == nil {
			return errorf(
				CodeInternal,
				"protocol error: client sent compressed Leading-Metadata envelope without compression support",
			)
		}
		decompressed := u.envelopeReader.bufferPool.Get()
		defer u.envelopeReader.bufferPool.Put(decompressed)
		if err := u.envelopeReader.compressionPool.Decompress(
			decompressed, data, int64(u.envelopeReader.readMaxBytes),
		); err != nil {
			return err
		}
		data = decompressed
	}
	if data.Len() == 0 {
		return nil
	}
	var meta map[string][]string
	if err := json.Unmarshal(data.Bytes(), &meta); err != nil {
		return errorf(CodeInvalidArgument, "unmarshal Leading-Metadata envelope: %w", err)
	}
	// RFC 008: envelope values overwrite same-key upgrade-header values.
	for key, values := range meta {
		canonical := http.CanonicalHeaderKey(key)
		u.requestHeader[canonical] = append([]string(nil), values...)
	}
	return nil
}

// Helpers.

func isWebSocketUpgrade(request *http.Request) bool {
	if !tokenListContainsFold(request.Header.Values(wsHeaderUpgrade), "websocket") {
		return false
	}
	return tokenListContainsFold(request.Header.Values(wsHeaderConnection), "upgrade")
}

func tokenListContainsFold(values []string, token string) bool {
	for _, value := range values {
		for _, candidate := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), token) {
				return true
			}
		}
	}
	return false
}

// pickConnectSubprotocol returns the first Connect-over-WebSocket subprotocol
// token offered by the client, in client-priority order.
func pickConnectSubprotocol(request *http.Request) (string, bool) {
	for _, value := range request.Header.Values(wsHeaderProtocol) {
		for _, token := range strings.Split(value, ",") {
			trimmed := strings.TrimSpace(token)
			switch trimmed {
			case wsSubprotocolProto, wsSubprotocolJSON, wsSubprotocolBase:
				return trimmed, true
			}
		}
	}
	return "", false
}

// chooseWebSocketCodec resolves the message codec from the negotiated
// subprotocol token and any Content-Type header. The two must agree if both
// are present (RFC 008).
func chooseWebSocketCodec(subprotocol, contentType string) (string, error) {
	var subCodec string
	switch subprotocol {
	case wsSubprotocolProto:
		subCodec = codecNameProto
	case wsSubprotocolJSON:
		subCodec = codecNameJSON
	}
	var headerCodec string
	if strings.HasPrefix(contentType, connectStreamingContentTypePrefix) {
		headerCodec = strings.TrimPrefix(contentType, connectStreamingContentTypePrefix)
		if i := strings.IndexByte(headerCodec, ';'); i >= 0 {
			headerCodec = strings.TrimSpace(headerCodec[:i])
		}
	}
	switch {
	case subCodec != "" && headerCodec != "" && subCodec != headerCodec:
		return "", fmt.Errorf(
			"Sec-WebSocket-Protocol subprotocol %q conflicts with Content-Type codec %q",
			subprotocol, headerCodec,
		)
	case subCodec != "":
		return subCodec, nil
	case headerCodec != "":
		return headerCodec, nil
	default:
		return "", fmt.Errorf(
			"cannot determine codec: set Sec-WebSocket-Protocol to %s or %s, or set Content-Type",
			wsSubprotocolProto, wsSubprotocolJSON,
		)
	}
}

func isCleanWebSocketClose(err error) bool {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseNormalClosure, websocket.CloseGoingAway:
			return true
		}
	}
	return false
}

// --- Client transport (Phase 4) -------------------------------------------
//
// The client mirrors the server: each binary frame carries exactly one
// envelope; envelopeWriter handles outgoing marshaling unchanged; a custom
// websocketClientUnmarshaler reads one envelope per frame and recognizes
// bit 1 (EndStream) the same way connectStreamingUnmarshaler does for HTTP
// streaming. Dialing is lazy — the first Send or Receive triggers it — so
// the existing client lifecycle (NewConn → Send → CloseRequest → Receive →
// CloseResponse) maps cleanly onto WebSocket framing.

type websocketClient struct {
	protocolClientParams

	dialURL *url.URL // url with scheme rewritten to ws/wss
	peer    Peer
}

func (c *websocketClient) Peer() Peer { return c.peer }

func (c *websocketClient) WriteRequestHeader(streamType StreamType, header http.Header) {
	_ = streamType // every WebSocket RPC uses streaming envelope framing
	if getHeaderCanonical(header, headerUserAgent) == "" {
		header[headerUserAgent] = []string{defaultConnectUserAgent}
	}
	// The transport carries Connect-Protocol-Version via the subprotocol
	// token; per RFC 008 clients SHOULD NOT send Connect-Protocol-Version and
	// servers MUST ignore it.
	header[headerContentType] = []string{connectStreamingContentTypePrefix + c.Codec.Name()}
	if c.CompressionName != "" && c.CompressionName != compressionIdentity {
		header[connectStreamingHeaderCompression] = []string{c.CompressionName}
	}
	if accept := c.CompressionPools.CommaSeparatedNames(); accept != "" {
		header[connectStreamingHeaderAcceptCompression] = []string{accept}
	}
	// Sec-WebSocket-Protocol is added by gorilla.Dialer.Subprotocols; gorilla
	// rejects setting it directly on the header.
}

func (c *websocketClient) NewConn(
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
			}
		}
	}
	call := &wsClientCall{
		ctx: ctx,
		dialer: &websocket.Dialer{
			// Honor the context, not a separate handshake timeout. Zero ==
			// no timeout at the dialer layer.
			Subprotocols: []string{wsSubprotocolBase},
		},
		url:      c.dialURL,
		header:   header,
		dialDone: make(chan struct{}),
	}
	conn := &websocketClientConn{
		spec:             spec,
		peer:             c.peer,
		call:             call,
		codec:            c.Codec,
		compressionPools: c.CompressionPools,
		marshaler: envelopeWriter{
			ctx:              ctx,
			sender:           call,
			codec:            c.Codec,
			compressMinBytes: c.CompressMinBytes,
			compressionPool:  c.CompressionPools.Get(c.CompressionName),
			bufferPool:       c.BufferPool,
			sendMaxBytes:     c.SendMaxBytes,
		},
		unmarshaler: websocketClientUnmarshaler{
			call:             call,
			codec:            c.Codec,
			compressionPools: c.CompressionPools,
			bufferPool:       c.BufferPool,
			readMaxBytes:     c.ReadMaxBytes,
		},
		responseHeader:  make(http.Header),
		responseTrailer: make(http.Header),
	}
	return wrapClientConnWithCodedErrors(conn)
}

// wsClientCall manages the lazy dial and serves as the messageSender that
// envelopeWriter writes to. Mirrors duplexHTTPCall's role for HTTP-based
// transports.
type wsClientCall struct {
	ctx    context.Context //nolint:containedctx
	dialer *websocket.Dialer
	url    *url.URL
	header http.Header

	onRequestSend func(*http.Request)

	dialOnce sync.Once
	dialDone chan struct{}
	wsConn   *websocket.Conn
	response *http.Response
	dialErr  *Error
}

func (c *wsClientCall) ensureDialed() *Error {
	c.dialOnce.Do(func() {
		defer close(c.dialDone)
		if c.onRequestSend != nil {
			// Build a stub *http.Request for onRequestSend visibility. gorilla
			// constructs its own request internally; this one is for the
			// callback only and is never sent.
			stub, _ := http.NewRequestWithContext(c.ctx, http.MethodGet, c.url.String(), http.NoBody)
			if stub != nil {
				for k, v := range c.header {
					stub.Header[k] = v
				}
				c.onRequestSend(stub)
			}
		}
		conn, response, err := c.dialer.DialContext(c.ctx, c.url.String(), c.header)
		c.response = response
		if err != nil {
			c.dialErr = dialError(err, response)
			return
		}
		if got := response.Header.Get(wsHeaderProtocol); got != wsSubprotocolBase {
			_ = conn.Close()
			c.dialErr = errorf(
				CodeInternal,
				"server selected unexpected Sec-WebSocket-Protocol %q (want %q)",
				got, wsSubprotocolBase,
			)
			return
		}
		c.wsConn = conn
		// gorilla.Conn does not honor a context during NextReader/NextWriter.
		// Translate context cancellation into an immediate read/write deadline
		// so callers waiting on a pending Receive observe the cancellation
		// promptly. The goroutine exits when the connection closes (deadline
		// set on a closed conn is harmless).
		go func() {
			<-c.ctx.Done()
			_ = conn.SetReadDeadline(time.Now())
			_ = conn.SetWriteDeadline(time.Now())
		}()
	})
	<-c.dialDone
	return c.dialErr
}

// Send implements messageSender for envelopeWriter. Writes one envelope as a
// single WebSocket binary message.
func (c *wsClientCall) Send(payload messagePayload) (int64, error) {
	if err := c.ensureDialed(); err != nil {
		return 0, err
	}
	writer, err := c.wsConn.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return 0, err
	}
	wroteN, writeErr := payload.WriteTo(writer)
	closeErr := writer.Close()
	if writeErr != nil {
		return wroteN, writeErr
	}
	return wroteN, closeErr
}

type websocketClientConn struct {
	spec    Spec
	peer    Peer
	call    *wsClientCall
	codec   Codec
	compressionPools readOnlyCompressionPools

	marshaler   envelopeWriter
	unmarshaler websocketClientUnmarshaler

	responseHeader  http.Header
	responseTrailer http.Header

	responseHeaderOnce sync.Once
	sendCloseOnce      sync.Once
	sendCloseErr       *Error
}

func (c *websocketClientConn) Spec() Spec { return c.spec }
func (c *websocketClientConn) Peer() Peer { return c.peer }

func (c *websocketClientConn) Send(msg any) error {
	if err := c.marshaler.Marshal(msg); err != nil {
		return err
	}
	return nil // literal nil; a nil *Error is a non-nil error
}

func (c *websocketClientConn) RequestHeader() http.Header {
	return c.call.header
}

func (c *websocketClientConn) CloseRequest() error {
	c.sendCloseOnce.Do(func() {
		// Send an empty bit-2 envelope to signal End-Of-Client-Stream.
		emptyBuffer := c.marshaler.bufferPool.Get()
		defer c.marshaler.bufferPool.Put(emptyBuffer)
		c.sendCloseErr = c.marshaler.Write(&envelope{
			Data:  emptyBuffer,
			Flags: wsFlagEnvelopeEndClientStream,
		})
	})
	if c.sendCloseErr != nil {
		return c.sendCloseErr
	}
	return nil
}

func (c *websocketClientConn) Receive(msg any) error {
	err := c.unmarshaler.Unmarshal(msg)
	if err == nil {
		return nil
	}
	// On EndStream, fold trailers and surface any server error.
	if c.unmarshaler.endStreamSeen {
		mergeHeaders(c.responseTrailer, c.unmarshaler.trailer)
		if serverErr := c.unmarshaler.endStreamError; serverErr != nil {
			serverErr.meta = c.ResponseHeader().Clone()
			mergeHeaders(serverErr.meta, c.responseTrailer)
			return serverErr
		}
	}
	return err
}

func (c *websocketClientConn) ResponseHeader() http.Header {
	c.responseHeaderOnce.Do(func() {
		if dialErr := c.call.ensureDialed(); dialErr != nil {
			return
		}
		if c.call.response == nil {
			return
		}
		for key, values := range c.call.response.Header {
			if isWebSocketProtocolHeader(key) {
				continue
			}
			c.responseHeader[key] = values
		}
	})
	return c.responseHeader
}

func (c *websocketClientConn) ResponseTrailer() http.Header {
	return c.responseTrailer
}

func (c *websocketClientConn) CloseResponse() error {
	if c.call.wsConn == nil {
		return nil
	}
	// Best-effort clean close. We send 1000 Normal Closure even if Receive
	// hasn't yet observed the EndStream — the server's normal completion path
	// already sent its own close, and gorilla's library handles the case
	// where the close frame round-trip is already done.
	_ = c.call.wsConn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(wsCloseWriteTimeout),
	)
	return c.call.wsConn.Close()
}

func (c *websocketClientConn) onRequestSend(fn func(*http.Request)) {
	c.call.onRequestSend = fn
}

// websocketClientUnmarshaler reads one envelope per WebSocket binary frame.
// It mirrors websocketUnmarshaler (server-side) but recognizes the server's
// bit-1 EndStream envelope instead of the client's bit-2/bit-3 flags.
type websocketClientUnmarshaler struct {
	call             *wsClientCall
	codec            Codec
	compressionPools readOnlyCompressionPools
	compressionPool  *compressionPool
	bufferPool       *bufferPool
	readMaxBytes     int

	compressionInitialized bool
	endStreamSeen          bool
	endStreamError         *Error
	trailer                http.Header
}

func (u *websocketClientUnmarshaler) Unmarshal(message any) *Error {
	if err := u.call.ensureDialed(); err != nil {
		u.endStreamSeen = true
		u.endStreamError = err
		return err
	}
	if !u.compressionInitialized {
		encoding := getHeaderCanonical(u.call.response.Header, connectStreamingHeaderCompression)
		if encoding != "" && encoding != compressionIdentity {
			pool := u.compressionPools.Get(encoding)
			if pool == nil {
				u.endStreamSeen = true
				u.endStreamError = errorf(
					CodeInternal,
					"server selected unsupported response compression %q", encoding,
				)
				return u.endStreamError
			}
			u.compressionPool = pool
		}
		u.compressionInitialized = true
	}
	if u.endStreamSeen {
		return NewError(CodeUnknown, io.EOF)
	}

	messageType, frame, readerErr := u.call.wsConn.NextReader()
	if readerErr != nil {
		u.endStreamSeen = true
		// If ctx is done, the read failure was almost certainly triggered by
		// our own SetReadDeadline-on-ctx-cancel goroutine. Surface ctx.Err so
		// callers see context.Canceled / context.DeadlineExceeded rather than
		// a generic transport "i/o timeout".
		if ctxErr := u.call.ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.Canceled) {
				u.endStreamError = errorf(CodeCanceled, "%w", ctxErr)
			} else {
				u.endStreamError = errorf(CodeDeadlineExceeded, "%w", ctxErr)
			}
			return u.endStreamError
		}
		if isCleanWebSocketClose(readerErr) {
			// Server closed without an EndStreamMessage. Per RFC 008 §Errors
			// this is treated as unavailable.
			u.endStreamError = errorf(
				CodeUnavailable,
				"server closed WebSocket without EndStreamMessage",
			)
			return u.endStreamError
		}
		return errorf(CodeUnavailable, "read websocket message: %w", readerErr)
	}
	if messageType != websocket.BinaryMessage {
		_, _ = io.Copy(io.Discard, frame)
		return errorf(
			CodeInvalidArgument,
			"Connect over WebSocket requires binary frames; got message type %d",
			messageType,
		)
	}

	buffer := u.bufferPool.Get()
	env := &envelope{Data: buffer}
	reader := &envelopeReader{
		ctx:             u.call.ctx,
		reader:          frame,
		codec:           u.codec,
		compressionPool: u.compressionPool,
		bufferPool:      u.bufferPool,
		readMaxBytes:    u.readMaxBytes,
	}
	if readErr := reader.Read(env); readErr != nil {
		u.bufferPool.Put(buffer)
		return readErr
	}
	if extra, _ := io.Copy(io.Discard, frame); extra > 0 {
		u.bufferPool.Put(buffer)
		return errorf(
			CodeInvalidArgument,
			"websocket frame contains %d extra bytes after envelope",
			extra,
		)
	}

	flags := env.Flags
	switch {
	case flags&(wsFlagEnvelopeEndClientStream|wsFlagEnvelopeLeadingMetadata) != 0:
		u.bufferPool.Put(buffer)
		return errorf(
			CodeInternal,
			"server sent envelope with client-only flags: 0x%02x", flags,
		)
	case flags&connectFlagEnvelopeEndStream != 0:
		defer u.bufferPool.Put(buffer)
		data := env.Data
		if env.IsSet(flagEnvelopeCompressed) {
			if u.compressionPool == nil {
				return errorf(
					CodeInternal,
					"server sent compressed EndStreamMessage without compression support",
				)
			}
			decompressed := u.bufferPool.Get()
			defer u.bufferPool.Put(decompressed)
			if err := u.compressionPool.Decompress(decompressed, data, int64(u.readMaxBytes)); err != nil {
				return err
			}
			data = decompressed
		}
		var end connectEndStreamMessage
		if data.Len() > 0 {
			if err := json.Unmarshal(data.Bytes(), &end); err != nil {
				return errorf(CodeInternal, "unmarshal EndStreamMessage: %w", err)
			}
		}
		for name, value := range end.Trailer {
			canonical := http.CanonicalHeaderKey(name)
			if name != canonical {
				delete(end.Trailer, name)
				end.Trailer[canonical] = append(end.Trailer[canonical], value...)
			}
		}
		u.endStreamSeen = true
		u.trailer = end.Trailer
		u.endStreamError = end.Error.asError()
		return NewError(CodeUnknown, io.EOF)
	case flags == 0 || flags == flagEnvelopeCompressed:
		defer u.bufferPool.Put(buffer)
		data := env.Data
		if env.IsSet(flagEnvelopeCompressed) {
			if u.compressionPool == nil {
				return errorf(
					CodeInternal,
					"server sent compressed envelope without compression support",
				)
			}
			decompressed := u.bufferPool.Get()
			defer u.bufferPool.Put(decompressed)
			if err := u.compressionPool.Decompress(decompressed, data, int64(u.readMaxBytes)); err != nil {
				return err
			}
			data = decompressed
		}
		if data.Len() == 0 {
			return nil
		}
		if err := u.codec.Unmarshal(data.Bytes(), message); err != nil {
			return errorf(CodeInvalidArgument, "unmarshal message: %w", err)
		}
		return nil
	default:
		u.bufferPool.Put(buffer)
		return errorf(
			CodeInvalidArgument,
			"server sent envelope with reserved flags: 0x%02x", flags,
		)
	}
}

func dialError(err error, response *http.Response) *Error {
	switch {
	case errors.Is(err, context.Canceled):
		return errorf(CodeCanceled, "WebSocket upgrade canceled: %w", err)
	case errors.Is(err, context.DeadlineExceeded):
		return errorf(CodeDeadlineExceeded, "WebSocket upgrade deadline exceeded: %w", err)
	}
	if response != nil {
		return errorf(
			httpToCode(response.StatusCode),
			"WebSocket upgrade failed: HTTP %s: %w",
			response.Status, err,
		)
	}
	return errorf(CodeUnavailable, "WebSocket upgrade failed: %w", err)
}

func isWebSocketProtocolHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Upgrade",
		"Connection",
		"Sec-Websocket-Accept",
		"Sec-Websocket-Protocol",
		"Sec-Websocket-Extensions",
		"Sec-Websocket-Version",
		"Sec-Websocket-Key":
		return true
	}
	return false
}
