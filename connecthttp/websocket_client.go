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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/coder/websocket"
)

// websocketClientConfig is the per-procedure state a WebSocket client stream
// needs. It is resolved once with the rest of the client state.
type websocketClientConfig struct {
	httpClient *http.Client
	url        url.URL
	codec      connect.Codec
	readLimit  int64
}

// newWebSocketClientConfig resolves the WebSocket state for one procedure.
func newWebSocketClientConfig(httpClient HTTPClient, procedureURL url.URL, opts *options) (*websocketClientConfig, error) {
	client, ok := httpClient.(*http.Client)
	if !ok {
		return nil, connect.Errorf(connect.CodeInternal, "WebSocket needs an *http.Client, got %T", httpClient)
	}
	switch procedureURL.Scheme {
	case "http":
		procedureURL.Scheme = "ws"
	case "https":
		procedureURL.Scheme = "wss"
	}
	return &websocketClientConfig{
		httpClient: client,
		url:        procedureURL,
		codec:      newReadOnlyCodecs(opts.codecs).Get(opts.getSendCodecName()),
		readLimit:  websocketReadLimit(opts.readMaxBytes),
	}, nil
}

// websocketClientStream is the [connect.ClientStream] for one RPC over
// WebSocket. It dials on first use. The send side and the receive side may
// run concurrently.
type websocketClientStream struct {
	ctx            context.Context //nolint:containedctx // the stream is the RPC's lifetime
	config         *websocketClientConfig
	info           *connect.CallInfo
	clientInfo     *ClientInfo
	singleRequest  bool
	singleResponse bool

	dialMu       sync.Mutex
	dialed       bool
	conn         *websocket.Conn
	dialErr      error
	stopWatching func() bool

	ended  atomic.Bool // S has arrived
	closed atomic.Bool // Close was called

	// Send side.
	sent       bool
	sendClosed bool
	// Receive side.
	receivedHeader bool
	receiveErr     error
}

func (s *websocketClientStream) SendHeaders() error {
	_, err := s.dial()
	return err
}

func (s *websocketClientStream) Send(msg any) error {
	conn, err := s.dial()
	if err != nil {
		return err
	}
	if s.sendClosed || s.ended.Load() {
		return io.EOF
	}
	if s.singleRequest && s.sent {
		return errors.New("connecthttp: unary stream sent more than once")
	}
	s.sent = true
	// A failed write means the connection is closing. Receive reports why.
	if err := writeWebSocketBody(context.WithoutCancel(s.ctx), conn, s.config.codec, msg); err != nil {
		if _, ok := asError(err); ok {
			return err
		}
		return io.EOF
	}
	return nil
}

func (s *websocketClientStream) CloseSend() error {
	conn, err := s.dial()
	if err != nil {
		return err
	}
	if s.sendClosed || s.ended.Load() {
		return nil
	}
	s.sendClosed = true
	_ = writeWebSocketText(context.WithoutCancel(s.ctx), conn, websocketMarkerClientEndStream, nil)
	return nil
}

func (s *websocketClientStream) Receive(msg any) error {
	if s.receiveErr != nil {
		return s.receiveErr
	}
	conn, err := s.dial()
	if err != nil {
		s.receiveErr = err
		return err
	}
	if !s.singleResponse {
		err := s.receive(conn, msg)
		if err != nil {
			s.receiveErr = err
		}
		return err
	}
	// Unary and client-streaming RPCs return exactly one response.
	s.receiveErr = io.EOF
	err = s.receive(conn, msg)
	if err == nil {
		if drainErr := s.receive(conn, msg); drainErr == nil {
			err = connect.Errorf(connect.CodeUnimplemented, "unary stream has multiple messages")
		} else if !errors.Is(drainErr, io.EOF) {
			err = drainErr
		}
	} else if errors.Is(err, io.EOF) {
		err = connect.Errorf(connect.CodeUnimplemented, "unary stream has zero messages")
	}
	if err != nil {
		s.receiveErr = err
	}
	return err
}

func (s *websocketClientStream) Close() error {
	s.closed.Store(true)
	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	if !s.dialed {
		// Never dial after Close.
		s.dialed = true
		s.dialErr = connect.NewError(connect.CodeCanceled, "stream closed")
		return nil
	}
	if s.conn != nil {
		s.release(websocket.StatusNormalClosure)
	}
	return nil
}

// dial opens the connection and sends M, once.
func (s *websocketClientStream) dial() (*websocket.Conn, error) {
	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	if s.dialed {
		return s.conn, s.dialErr
	}
	s.dialed = true
	s.conn, s.dialErr = s.connect()
	return s.conn, s.dialErr
}

func (s *websocketClientStream) connect() (*websocket.Conn, error) {
	// Check the metadata before dialing, so a bad key sends nothing.
	header := make(http.Header)
	toHTTPHeader(header, s.info.RequestHeader())
	metadata := encodeWebSocketMetadata(header)
	if _, err := decodeWebSocketMetadata(metadata, true); err != nil {
		return nil, err
	}
	requestURL := s.config.url
	if deadline, ok := s.ctx.Deadline(); ok {
		if timeout := connectEncodeTimeout(time.Until(deadline)); timeout != "" {
			requestURL.RawQuery = url.Values{websocketTimeoutQueryParameter: {timeout}}.Encode()
		}
	}
	// A shallow copy whose transport records the handshake on ClientInfo.
	httpClient := *s.config.httpClient
	httpClient.Transport = &websocketRecordingTransport{next: httpClient.Transport, info: s.clientInfo}
	conn, response, err := websocket.Dial(s.ctx, requestURL.String(), &websocket.DialOptions{
		HTTPClient:   &httpClient,
		HTTPHeader:   http.Header{headerUserAgent: {defaultConnectUserAgent}},
		Subprotocols: []string{websocketSubprotocol(s.config.codec.Name())},
	})
	// coder clears the body after an upgrade and keeps a prefix after a
	// failure.
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return nil, s.dialError(response, err)
	}
	if conn.Subprotocol() == "" {
		_ = conn.Close(websocketStatusClientError, "")
		return nil, connect.NewError(connect.CodeInternal, "server accepted the WebSocket without a subprotocol")
	}
	conn.SetReadLimit(s.config.readLimit)
	s.stopWatching = context.AfterFunc(s.ctx, func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	if err := writeWebSocketText(context.WithoutCancel(s.ctx), conn, websocketMarkerLeadingMetadata, metadata); err != nil {
		s.stopWatching()
		_ = conn.CloseNow()
		return nil, s.readError(err)
	}
	return conn, nil
}

// dialError maps a failed handshake to an error.
func (s *websocketClientStream) dialError(response *http.Response, err error) error {
	if response == nil {
		if err := wrapIfContextDone(s.ctx, err); err != nil {
			if _, ok := asError(err); ok {
				return err
			}
		}
		return connect.Errorf(connect.CodeUnavailable, "dial WebSocket: %s", err).WithCause(err)
	}
	if response.StatusCode == http.StatusSwitchingProtocols {
		// The upgrade happened but broke the WebSocket rules, such as an
		// unoffered subprotocol.
		return connect.Errorf(connect.CodeInternal, "dial WebSocket: %s", err).WithCause(err)
	}
	// coder keeps the first 1024 bytes of the body.
	var wireErr connectWireError
	if body, readErr := io.ReadAll(response.Body); readErr == nil && json.Unmarshal(body, &wireErr) == nil && wireErr.Code != 0 {
		return wireErr.asError()
	}
	return connect.Errorf(httpToCode(response.StatusCode), "dial WebSocket: HTTP status %d", response.StatusCode).WithCause(err)
}

// receive reads the next B into msg. It handles M and S on the way.
func (s *websocketClientStream) receive(conn *websocket.Conn, msg any) error {
	bodyIsText := websocketBodyIsText(s.config.codec.Name())
	for {
		messageType, data, err := conn.Read(context.WithoutCancel(s.ctx))
		if err != nil {
			return s.readError(err)
		}
		message, parseErr := parseWebSocketMessage(messageType, data, bodyIsText)
		if parseErr != nil {
			return s.fail(parseErr)
		}
		if !s.receivedHeader && message.marker != websocketMarkerLeadingMetadata {
			return s.fail(newWebSocketProtocolError("first message is %c, expected M", message.marker))
		}
		switch message.marker {
		case websocketMarkerLeadingMetadata:
			if s.receivedHeader {
				return s.fail(newWebSocketProtocolError("second M message"))
			}
			s.receivedHeader = true
			header, err := decodeWebSocketMetadata(message.payload, false)
			if err != nil {
				return s.fail(err)
			}
			fromHTTPHeader(s.info.ResponseHeader(), header)
		case websocketMarkerBody:
			if err := s.config.codec.UnmarshalRead(s.ctx, bytes.NewReader(message.payload), msg); err != nil {
				return connect.Errorf(connect.CodeInvalidArgument, "unmarshal message: %s", err).WithCause(err)
			}
			return nil
		case websocketMarkerServerEndStream:
			return s.receiveEnd(message.payload)
		default:
			return s.fail(newWebSocketProtocolError("unexpected %c message from server", message.marker))
		}
	}
}

// receiveEnd handles S, which ends the RPC, and closes the connection.
func (s *websocketClientStream) receiveEnd(payload []byte) error {
	var end connectEndStreamMessage
	if err := json.Unmarshal(payload, &end); err != nil {
		return s.fail(newWebSocketProtocolError("S message is invalid: %v", err))
	}
	for key, values := range end.Trailer {
		s.info.ResponseTrailer().SetValues(http.CanonicalHeaderKey(key), values)
	}
	s.ended.Store(true)
	s.release(websocket.StatusNormalClosure)
	if end.Error != nil {
		return end.Error.asError()
	}
	return io.EOF
}

// fail closes the connection after a protocol error from the server.
func (s *websocketClientStream) fail(err *connect.Error) error {
	code := websocketStatusClientError
	if errors.Is(err, errWebSocketInvalidUTF8) {
		code = websocket.StatusInvalidFramePayloadData
	}
	s.release(code)
	return err
}

// release stops watching the context and closes the connection with code.
func (s *websocketClientStream) release(code websocket.StatusCode) {
	s.stopWatching()
	_ = s.conn.Close(code, "")
}

// readError maps a connection that ended without S to an error.
func (s *websocketClientStream) readError(err error) error {
	if s.closed.Load() {
		return connect.NewError(connect.CodeCanceled, "stream closed")
	}
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return wrapIfContextError(ctxErr)
	}
	switch {
	case errors.Is(err, websocket.ErrMessageTooBig), websocket.CloseStatus(err) == websocket.StatusMessageTooBig:
		return connect.Errorf(connect.CodeResourceExhausted, "message too big: %s", err).WithCause(err)
	case websocket.CloseStatus(err) == websocket.StatusInvalidFramePayloadData:
		return connect.Errorf(connect.CodeInternal, "server rejected a message: %s", err).WithCause(err)
	default:
		return connect.Errorf(connect.CodeUnavailable, "connection closed before end of stream: %s", err).WithCause(err)
	}
}

// websocketRecordingTransport records the handshake request and response on
// [ClientInfo].
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
