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
	cancel         context.CancelFunc
	config         *websocketClientConfig
	info           *connect.CallInfo
	clientInfo     *ClientInfo
	singleRequest  bool
	singleResponse bool
	closed         atomic.Bool

	dialMu       sync.Mutex
	conn         *websocket.Conn
	dialErr      error
	stopWatching func() bool

	// Send side.
	sent       bool
	sendClosed bool
	// Receive side.
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
	// Close cancels ctx, which also stops a dial in progress.
	ctx, cancel := context.WithCancel(ctx)
	return &websocketClientStream{
		ctx:            ctx,
		cancel:         cancel,
		config:         config,
		info:           info,
		clientInfo:     clientInfo,
		singleRequest:  spec.StreamType == connect.StreamTypeUnary || spec.StreamType == connect.StreamTypeServer,
		singleResponse: spec.StreamType == connect.StreamTypeUnary || spec.StreamType == connect.StreamTypeClient,
	}
}

func (s *websocketClientStream) SendHeaders() error {
	_, err := s.dial()
	return err
}

// Send writes a B message. Once the connection is closing, it returns io.EOF
// and Receive reports why.
func (s *websocketClientStream) Send(msg any) error {
	conn, err := s.dial()
	if err != nil {
		return err
	}
	if s.sendClosed {
		return io.EOF
	}
	if s.singleRequest && s.sent {
		return errors.New("connecthttp: unary stream sent more than once")
	}
	s.sent = true
	if err := writeWebSocketBody(context.WithoutCancel(s.ctx), conn, s.config.codec, msg); err != nil {
		if _, ok := asError(err); ok {
			return err
		}
		return io.EOF
	}
	return nil
}

// CloseSend writes a C message. A failed write means the connection is
// closing, and Receive reports why.
func (s *websocketClientStream) CloseSend() error {
	conn, err := s.dial()
	if err != nil {
		return err
	}
	if s.sendClosed {
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
	receive := func(msg any) error { return s.receive(conn, msg) }
	if s.singleResponse {
		// Unary and client-streaming RPCs return exactly one response.
		s.receiveErr = io.EOF
		err = receiveExactlyOne(receive, msg, "response")
	} else {
		err = receive(msg)
	}
	if err != nil {
		s.receiveErr = err
		// The RPC is over. After S the connection is already closed.
		closeCode := websocketStatusClientError
		if errors.Is(err, errWebSocketInvalidUTF8) {
			closeCode = websocket.StatusInvalidFramePayloadData
		}
		s.release(closeCode)
	}
	return err
}

// Close ends the RPC. The connection closes with 1000 in the background.
func (s *websocketClientStream) Close() error {
	s.closed.Store(true)
	s.cancel()
	return nil
}

// dial opens the connection and sends M, once.
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
	if s.closed.Load() {
		return nil, connect.NewError(connect.CodeCanceled, "stream closed")
	}
	// Check the metadata before dialing, so a bad key sends nothing.
	metadata := encodeWebSocketMetadata(websocketMetadata(s.info.RequestHeader()))
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
		Subprotocols: []string{websocketSubprotocolPrefix + s.config.codec.Name()},
	})
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
		if connectErr, ok := asError(wrapIfContextDone(s.ctx, err)); ok {
			return connectErr
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
			return parseErr
		}
		if !s.receivedHeader && message.marker != websocketMarkerLeadingMetadata {
			return newWebSocketProtocolError("first message is %c, expected M", message.marker)
		}
		switch message.marker {
		case websocketMarkerLeadingMetadata:
			if s.receivedHeader {
				return newWebSocketProtocolError("second M message")
			}
			s.receivedHeader = true
			header, err := decodeWebSocketMetadata(message.payload, false)
			if err != nil {
				return err
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
			return newWebSocketProtocolError("unexpected %c message from server", message.marker)
		}
	}
}

// receiveEnd handles S, which ends the RPC, and closes the connection.
func (s *websocketClientStream) receiveEnd(payload []byte) error {
	var end connectEndStreamMessage
	if err := json.Unmarshal(payload, &end); err != nil {
		return newWebSocketProtocolError("S message is invalid: %v", err)
	}
	trailer := make(http.Header, len(end.Trailer))
	for key, values := range end.Trailer {
		for _, value := range values {
			trailer.Add(key, value)
		}
	}
	fromHTTPHeader(s.info.ResponseTrailer(), trailer)
	s.release(websocket.StatusNormalClosure)
	if end.Error != nil {
		return end.Error.asError()
	}
	return io.EOF
}

// release closes the connection with closeCode and ends the stream's
// context. Calls after the first do nothing.
func (s *websocketClientStream) release(closeCode websocket.StatusCode) {
	// Stop the context watcher first, so it can't close with 1000 instead.
	s.stopWatching()
	_ = s.conn.Close(closeCode, "")
	s.cancel()
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
