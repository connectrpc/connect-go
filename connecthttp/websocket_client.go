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

func newWebSocketClientConfig(httpClient HTTPClient, procedureURL url.URL, opts *options) *websocketClientConfig {
	client, ok := httpClient.(*http.Client)
	if !ok {
		// coder only takes an *http.Client. The wrapped client follows
		// redirects with its own policy.
		client = &http.Client{
			Transport: &websocketDoTransport{httpClient: httpClient},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &websocketClientConfig{
		httpClient:   client,
		url:          procedureURL,
		codec:        newReadOnlyCodecs(opts.codecs).Get(opts.getSendCodecName()),
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
	clientInfo     *ClientInfo
	singleRequest  bool
	singleResponse bool

	dialMu       sync.Mutex
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
	clientInfo *ClientInfo,
) *websocketClientStream {
	info.Spec = spec
	info.PeerAddr = config.url.Host
	info.Protocol = protocolNameWebSocket
	info.Codec = config.codec.Name()
	info.RequestEncoding = connect.CompressionNameIdentity
	info.ResponseEncoding = connect.CompressionNameIdentity
	// Every path cancels ctx, which ends ioCtx, so its cancel is unneeded.
	ctx, cancel := context.WithCancel(ctx)
	ioCtx, _ := websocketGraceContext(ctx)
	return &websocketClientStream{
		ctx:            ctx,
		ioCtx:          ioCtx,
		cancel:         cancel,
		config:         config,
		info:           info,
		clientInfo:     clientInfo,
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
	// Marshal before dialing, so a message over the limit sends nothing.
	body, err := websocketMarshalBody(s.ctx, s.config.codec, msg, s.config.sendMaxBytes)
	if err != nil {
		return err
	}
	defer bufferpool.Put(body)
	conn, err := s.dial()
	if err != nil {
		return err
	}
	s.sent = true
	if err := conn.Write(s.ioCtx, websocketBodyType(s.config.codec.Name()), body.Bytes()); err != nil {
		return io.EOF
	}
	if s.singleRequest {
		// A unary request is always followed by C.
		_ = s.CloseSend()
	}
	return nil
}

func (s *websocketClientStream) CloseSend() error {
	conn, err := s.dial()
	if err != nil {
		return err
	}
	if s.sendClosed {
		return nil
	}
	select {
	case <-s.ctx.Done():
		// After S, or once the stream is canceled or failed, a client may
		// send only a Close frame.
		return nil
	default:
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
		if errors.Is(err, errWebSocketInvalidUTF8) {
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

func (s *websocketClientStream) dial() (*websocket.Conn, error) {
	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	if s.conn == nil && s.dialErr == nil {
		s.conn, s.dialErr = s.connect()
		if s.dialErr != nil {
			s.cancel()
		}
	}
	return s.conn, s.dialErr
}

func (s *websocketClientStream) connect() (*websocket.Conn, error) {
	metadata := websocketMetadata(s.info.RequestHeader())
	handshakeHeader := websocketHandshakeHeader(s.info.RequestHeader(), metadata)
	// Check the metadata before dialing, so a bad key sends nothing.
	for key, values := range metadata {
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
	// Copy so the caller's client is left unchanged.
	httpClient := *s.config.httpClient
	httpClient.Transport = &websocketRecordingTransport{next: httpClient.Transport, info: s.clientInfo}
	subprotocol := websocketSubprotocolPrefix + s.config.codec.Name()
	conn, response, err := websocket.Dial(s.ctx, requestURL.String(), &websocket.DialOptions{
		HTTPClient:   &httpClient,
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
		// The handshake failed, so there's no WebSocket to send a Close frame on.
		_ = conn.CloseNow()
		return nil, connect.Errorf(connect.CodeInternal, "server echoed subprotocol %q, expected %q", echoed, subprotocol)
	}
	conn.SetReadLimit(s.config.readLimit)
	s.stopWatching = context.AfterFunc(s.ctx, func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	buffer.WriteByte(websocketMarkerLeadingMetadata)
	websocketEncodeMetadata(buffer, metadata)
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
	// Metadata keys are lower-case, not canonical.
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

// receive runs after dial, which publishes s.conn under dialMu.
func (s *websocketClientStream) receive(msg any) error {
	for {
		marker, err := s.receiveMessage(msg)
		if err != nil || marker != websocketMarkerLeadingMetadata {
			return err
		}
	}
}

// receiveMessage handles one message and returns its marker.
func (s *websocketClientStream) receiveMessage(msg any) (byte, error) {
	buffer := bufferpool.Get()
	defer bufferpool.Put(buffer)
	messageType, err := websocketReadMessage(s.ioCtx, s.conn, buffer)
	if err != nil {
		return 0, s.readError(err)
	}
	// ioCtx outlives ctx, so a message can still arrive after cancel.
	if err := s.ctx.Err(); err != nil {
		return 0, s.readError(err)
	}
	marker, parseErr := websocketParseMarker(messageType, buffer.Bytes(), websocketBodyType(s.config.codec.Name()))
	if parseErr != nil {
		return 0, parseErr
	}
	if !s.receivedHeader && marker != websocketMarkerLeadingMetadata {
		return 0, newWebSocketProtocolError("first message is %c, expected M", marker)
	}
	// Skip the marker, leaving the payload.
	buffer.Next(1)
	switch marker {
	case websocketMarkerLeadingMetadata:
		if s.receivedHeader {
			return 0, newWebSocketProtocolError("second M message")
		}
		s.receivedHeader = true
		header, err := websocketDecodeMetadata(buffer.Bytes(), false)
		if err != nil {
			return 0, err
		}
		fromHTTPHeader(s.info.ResponseHeader(), header)
	case websocketMarkerBody:
		if err := s.config.codec.UnmarshalRead(s.ctx, buffer, msg); err != nil {
			return 0, newWebSocketDecodeError(err)
		}
	case websocketMarkerServerEndStream:
		return 0, s.receiveEnd(buffer.Bytes())
	default:
		return 0, newWebSocketProtocolError("unexpected %c message from server", marker)
	}
	return marker, nil
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
	// False once the context watcher or an earlier release has closed.
	if s.stopWatching() {
		// Don't make the caller wait for the close handshake, up to 5s in coder.
		go func() { _ = s.conn.Close(closeCode, reason) }()
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

type websocketRecordingTransport struct {
	next http.RoundTripper
	info *ClientInfo
}

func (t *websocketRecordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.info.request.Store(request)
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	response, err := next.RoundTrip(request)
	if response != nil {
		t.info.response.Store(&clientResponseInfo{
			status: response.StatusCode,
			proto:  response.Proto,
			tls:    response.TLS,
		})
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
