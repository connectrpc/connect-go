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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/bufferpool"
	"github.com/coder/websocket"
)

type websocketClientConfig struct {
	httpClient   *http.Client
	url          url.URL
	codec        connect.Codec
	readLimit    int64
	sendMaxBytes int
}

func newWebSocketClientConfig(
	httpClient HTTPClient,
	procedureURL url.URL,
	codec connect.Codec,
	opts *options,
) *websocketClientConfig {
	// The handshake is recorded on ClientInfo by wrapping the Transport, which
	// needs a copy of the caller's client. Unlike on the HTTP protocols, a
	// change to the caller's client may not reach later WebSocket calls.
	var client http.Client
	if stdClient, ok := httpClient.(*http.Client); ok {
		client = *stdClient
	} else {
		// coder only takes an *http.Client. The wrapped client follows
		// redirects with its own policy.
		client = http.Client{
			Transport: &websocketDoTransport{httpClient: httpClient},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	next := client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	client.Transport = &websocketRecordingTransport{next: next}
	return &websocketClientConfig{
		httpClient:   &client,
		url:          procedureURL,
		codec:        codec,
		readLimit:    websocketReadLimit(opts.readMaxBytes),
		sendMaxBytes: opts.sendMaxBytes,
	}
}

// websocketClientStream's send and receive sides may run concurrently.
type websocketClientStream struct {
	ctx            context.Context //nolint:containedctx // the stream is the RPC's lifetime
	ioCtx          context.Context //nolint:containedctx // ends shortly after ctx, see websocketGraceContext
	cancel         context.CancelFunc
	config         *websocketClientConfig
	info           *connect.CallInfo
	singleRequest  bool
	singleResponse bool

	dialOnce     sync.Once
	conn         *websocket.Conn
	dialErr      error
	stopWatching func() bool

	sent           bool
	sendClosed     bool
	receivedHeader bool
	receiveErr     error
}

func newWebSocketClientStream(
	ctx context.Context,
	spec connect.Spec,
	config *websocketClientConfig,
	info *connect.CallInfo,
) *websocketClientStream {
	info.Spec = spec
	info.PeerAddr = config.url.Host
	info.Protocol = protocolNameWebSocket
	info.Codec = config.codec.Name()
	info.RequestEncoding = connect.CompressionNameIdentity
	info.ResponseEncoding = connect.CompressionNameIdentity
	ctx, cancel := context.WithCancel(ctx)
	ioCtx, _ := websocketGraceContext(ctx)
	return &websocketClientStream{
		ctx:            ctx,
		ioCtx:          ioCtx,
		cancel:         cancel,
		config:         config,
		info:           info,
		singleRequest:  spec.StreamType&connect.StreamTypeClient == 0,
		singleResponse: spec.StreamType&connect.StreamTypeServer == 0,
	}
}

func (s *websocketClientStream) SendHeaders() error {
	_, err := s.dial()
	return err
}

// Send returns io.EOF once the connection is closing. Receive reports why.
func (s *websocketClientStream) Send(msg any) error {
	if s.singleRequest && s.sent {
		return errors.New("connecthttp: unary stream sent more than once")
	}
	if s.sendClosed || s.ctx.Err() != nil {
		return io.EOF
	}
	body := bufferpool.Get()
	defer bufferpool.Put(body)
	if err := websocketMarshalBody(s.ctx, body, s.config.codec, msg, s.config.sendMaxBytes); err != nil {
		return err
	}
	conn, err := s.dial()
	if err != nil {
		return err
	}
	s.sent = true
	if err := conn.Write(s.ioCtx, websocketBodyType(s.config.codec.Name()), body.Bytes()); err != nil {
		return io.EOF
	}
	if s.singleRequest {
		_ = s.CloseSend()
	}
	return nil
}

func (s *websocketClientStream) CloseSend() error {
	conn, err := s.dial()
	if err != nil {
		return err
	}
	if s.sendClosed || s.ctx.Err() != nil {
		return nil //nolint:nilerr // the context error is reported by Receive
	}
	s.sendClosed = true
	_ = conn.Write(s.ioCtx, websocket.MessageText, []byte{websocketMarkerClientEndStream})
	return nil
}

func (s *websocketClientStream) Receive(msg any) error {
	if s.receiveErr != nil {
		return s.receiveErr
	}
	if _, err := s.dial(); err != nil {
		s.receiveErr = err
		return err
	}
	var err error
	if s.singleResponse {
		err = receiveExactlyOne(s.receive, msg, "response")
		if err == nil {
			s.receiveErr = io.EOF
		}
	} else {
		err = s.receive(msg)
	}
	if err != nil {
		s.receiveErr = err
		closeCode := websocketStatusClientError
		switch {
		case errors.Is(err, websocket.ErrMessageTooBig):
			closeCode = websocket.StatusMessageTooBig
		case errors.Is(err, errWebSocketInvalidUTF8):
			closeCode = websocket.StatusInvalidFramePayloadData
		}
		s.release(closeCode, websocketCloseReason(err.Error()))
	}
	return err
}

func (s *websocketClientStream) Close() error {
	s.cancel()
	return nil
}

// dial runs the handshake once.
func (s *websocketClientStream) dial() (*websocket.Conn, error) {
	s.dialOnce.Do(func() {
		s.conn, s.dialErr = s.handshake()
		if s.dialErr != nil {
			s.cancel()
		}
	})
	return s.conn, s.dialErr
}

func (s *websocketClientStream) handshake() (*websocket.Conn, error) {
	metadata := websocketMetadata(s.info.RequestHeader())
	handshakeHeader := websocketHandshakeHeader(s.info.RequestHeader(), metadata)
	for key, values := range metadata {
		// As with net/http, a client fails on metadata a server would drop.
		if err := websocketValidateMetadata(key, values, true); err != nil {
			return nil, err
		}
	}
	requestURL := s.config.url
	if deadline, ok := s.ctx.Deadline(); ok {
		if timeout := connectEncodeTimeout(time.Until(deadline)); timeout != "" {
			requestURL.RawQuery = url.Values{websocketTimeoutQueryParameter: {timeout}}.Encode()
		}
	}
	subprotocol := websocketSubprotocolPrefix + s.config.codec.Name()
	conn, response, err := websocket.Dial(s.ctx, requestURL.String(), &websocket.DialOptions{
		HTTPClient:   s.config.httpClient,
		HTTPHeader:   handshakeHeader,
		Subprotocols: []string{subprotocol},
	})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return nil, s.dialError(response, err)
	}
	// coder accepts no echo, or an echo that differs only in case.
	if echoed := conn.Subprotocol(); echoed != subprotocol {
		// A missing or unoffered echo fails the handshake. Spec §4.3 has a
		// non-browser client fail the connection without a Close frame.
		_ = conn.CloseNow()
		return nil, connect.Errorf(connect.CodeInternal, "server echoed subprotocol %q, expected %q", echoed, subprotocol)
	}
	conn.SetReadLimit(s.config.readLimit)
	// A canceled call doesn't wait for the close handshake. coder would read the
	// rest of a message the server is sending with no deadline.
	s.stopWatching = context.AfterFunc(s.ctx, func() {
		_ = conn.CloseNow()
	})
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	websocketMarshalMetadata(buffer, metadata)
	if err := conn.Write(s.ioCtx, websocket.MessageText, buffer.Bytes()); err != nil {
		s.stopWatching()
		_ = conn.CloseNow()
		return nil, s.readError(err)
	}
	return conn, nil
}

// websocketHandshakeHeader moves the caller's User-Agent from metadata to the
// handshake, since the server rejects a metadata key that is also a handshake
// header. As on Connect, a key without values sends no User-Agent at all.
func websocketHandshakeHeader(requestHeader *connect.Header, metadata http.Header) http.Header {
	if !requestHeader.Has(headerUserAgent) {
		return http.Header{headerUserAgent: {defaultConnectUserAgent}}
	}
	delete(metadata, strings.ToLower(headerUserAgent))
	return http.Header{headerUserAgent: requestHeader.Values(headerUserAgent)}
}

func (s *websocketClientStream) dialError(response *http.Response, err error) error {
	cause := websocketCause(err)
	// A 101 means coder rejected the upgrade, such as for an unoffered
	// subprotocol.
	if errors.Is(err, errWebSocketHTTPClient) || (response != nil && response.StatusCode == http.StatusSwitchingProtocols) {
		return connect.Errorf(connect.CodeInternal, "dial WebSocket: %s", err).WithCause(cause)
	}
	if response == nil {
		if connectErr, ok := asError(wrapIfContextDone(s.ctx, err)); ok {
			return connectErr
		}
		return connect.Errorf(connect.CodeUnavailable, "dial WebSocket: %s", err).WithCause(cause)
	}
	// coder keeps the first 1024 bytes of the body.
	var wireErr connectWireError
	if body, readErr := io.ReadAll(response.Body); readErr == nil && json.Unmarshal(body, &wireErr) == nil && wireErr.Code != 0 {
		return wireErr.asError()
	}
	return connect.Errorf(httpToCode(response.StatusCode), "dial WebSocket: HTTP status %d", response.StatusCode).WithCause(cause)
}

// receive runs after a successful dial.
func (s *websocketClientStream) receive(msg any) error {
	conn, _ := s.dial()
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	for {
		buffer.Reset()
		messageType, err := websocketReadMessage(s.ioCtx, conn, buffer)
		if err != nil {
			return s.readError(err)
		}
		if err := s.ctx.Err(); err != nil {
			return wrapIfContextError(err)
		}
		marker, parseErr := websocketParseMarker(messageType, buffer, s.config.codec.Name())
		if parseErr != nil {
			return parseErr
		}
		if !s.receivedHeader && marker != websocketMarkerLeadingMetadata {
			return newWebSocketProtocolError("first message is %c, expected M", marker)
		}
		switch marker {
		case websocketMarkerLeadingMetadata:
			if s.receivedHeader {
				return newWebSocketProtocolError("second M message")
			}
			s.receivedHeader = true
			header, decodeErr := websocketDecodeMetadata(buffer.Bytes(), false)
			if decodeErr != nil {
				return decodeErr
			}
			fromHTTPHeader(s.info.ResponseHeader(), header)
		case websocketMarkerBody:
			if err := s.config.codec.UnmarshalRead(s.ctx, buffer, msg); err != nil {
				return newWebSocketDecodeError(err)
			}
			return nil
		case websocketMarkerServerEndStream:
			return s.receiveEnd(buffer.Bytes())
		default:
			return newWebSocketProtocolError("unexpected %c message from server", marker)
		}
	}
}

func (s *websocketClientStream) receiveEnd(payload []byte) error {
	var end *connectEndStreamMessage
	if err := json.Unmarshal(payload, &end); err != nil {
		return newWebSocketProtocolError("S message is invalid: %v", err)
	} else if end == nil {
		return newWebSocketProtocolError("S message is null, expected an object")
	}
	trailer := make(http.Header, len(end.Trailer))
	for key, values := range end.Trailer {
		if err := websocketValidateTrailer(key, values); err != nil {
			return err
		}
		for _, value := range values {
			trailer.Add(key, value)
		}
	}
	fromHTTPHeader(s.info.ResponseTrailer(), trailer)
	s.release(websocket.StatusNormalClosure, "")
	if end.Error != nil {
		return end.Error.asError()
	}
	return io.EOF
}

// release closes with the first code it's given. After S, Receive releases
// again with the RPC's outcome, which must not replace 1000.
func (s *websocketClientStream) release(closeCode websocket.StatusCode, reason string) {
	if s.stopWatching() {
		conn, _ := s.dial()
		if closeCode == websocket.StatusMessageTooBig {
			// coder already sent 1009. Close would drain the rest of the
			// message with no deadline and hang on a server that stalls.
			_ = conn.CloseNow()
		} else {
			// Don't make the caller wait for the close handshake.
			go func() { _ = conn.Close(closeCode, reason) }()
		}
	}
	s.cancel()
}

func (s *websocketClientStream) readError(err error) error {
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return wrapIfContextError(ctxErr)
	}
	err = websocketCause(err)
	switch {
	case errors.Is(err, websocket.ErrMessageTooBig), websocket.CloseStatus(err) == websocket.StatusMessageTooBig:
		return connect.Errorf(connect.CodeResourceExhausted, "message too big: %s", err).WithCause(err)
	case websocket.CloseStatus(err) == websocket.StatusInvalidFramePayloadData:
		return connect.Errorf(connect.CodeInternal, "server rejected a message: %s", err).WithCause(err)
	default:
		return connect.Errorf(connect.CodeUnavailable, "connection closed before end of stream: %s", err).WithCause(err)
	}
}

// websocketRecordingTransport records the handshake on the [ClientInfo] of
// the RPC that dials, found through the request's context.
type websocketRecordingTransport struct {
	next http.RoundTripper
}

func (t *websocketRecordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	info, _ := ClientInfoForContext(request.Context())
	if info != nil {
		info.request.Store(request)
	}
	response, err := t.next.RoundTrip(request)
	if info != nil && response != nil {
		info.response.Store(response)
	}
	return response, err
}

// websocketDoTransport adapts an [HTTPClient] that isn't an [*http.Client].
// The upgrade needs the 101 response body to be writable. A client with a
// Timeout, or one that buffers or wraps the body, breaks that, so it fails
// here with a clear error rather than inside coder.
type websocketDoTransport struct {
	httpClient HTTPClient
}

func (t *websocketDoTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.httpClient.Do(request)
	switch {
	case err != nil:
		// net/http logs a response returned with an error, and drops it.
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, err
	case response == nil:
		return nil, fmt.Errorf("%w: no response", errWebSocketHTTPClient)
	case response.StatusCode != http.StatusSwitchingProtocols:
		return response, nil
	}
	if _, ok := response.Body.(io.ReadWriteCloser); !ok {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, fmt.Errorf("%w: the 101 response body %T can't be written", errWebSocketHTTPClient, response.Body)
	}
	return response, nil
}

// websocketMetadata converts request headers to M metadata. The caller
// validates it.
func websocketMetadata(header *connect.Header) http.Header {
	metadata := make(http.Header)
	for key, values := range userHeaders(header) {
		key = strings.ToLower(key)
		if len(values) == 0 || strings.HasPrefix(key, "connect-") {
			continue
		}
		for _, value := range values {
			metadata[key] = append(metadata[key], websocketTrimMetadataValue(value))
		}
	}
	return metadata
}

// websocketCloseReason truncates reason to fit a Close frame. coder sends no
// Close frame for a reason over 123 bytes.
func websocketCloseReason(reason string) string {
	const maxReasonBytes = 123
	if len(reason) <= maxReasonBytes {
		return reason
	}
	return strings.ToValidUTF8(reason[:maxReasonBytes], "")
}
