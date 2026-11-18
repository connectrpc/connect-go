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

package wstransport_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
)

// pingOverWebSocket makes one unary call to baseURL with everything upgraded,
// and returns its error.
func pingOverWebSocket(tb testing.TB, httpServer *httptest.Server) error {
	tb.Helper()
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	_, err := client.Ping(tb.Context(), &pingv1.PingRequest{Number: 1})
	return err
}

// §4.5: a non-browser client that is refused reads the response. A Connect
// error in the body wins; otherwise the status maps as over HTTP.
func TestRejectedHandshakeReportsTheServersError(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		status      int
		contentType string
		body        string
		code        connect.Code
		mention     string
	}{
		{
			// 400 alone would map to internal, so the body is what decided.
			name:        "Connect error in the body",
			status:      http.StatusBadRequest,
			contentType: "application/json",
			body:        `{"code":"unauthenticated","message":"token expired"}`,
			code:        connect.CodeUnauthenticated,
			mention:     "token expired",
		},
		{
			name:        "plain body",
			status:      http.StatusForbidden,
			contentType: "text/plain",
			body:        "origin not allowed",
			code:        connect.CodePermissionDenied,
		},
		{
			name:        "JSON that is not a Connect error",
			status:      http.StatusNotFound,
			contentType: "application/json",
			body:        `{"detail":"nope"}`,
			code:        connect.CodeUnimplemented,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := httptest.NewServer(http.HandlerFunc(
				func(responseWriter http.ResponseWriter, _ *http.Request) {
					responseWriter.Header().Set("Content-Type", test.contentType)
					responseWriter.WriteHeader(test.status)
					_, _ = responseWriter.Write([]byte(test.body))
				},
			))
			t.Cleanup(httpServer.Close)

			err := pingOverWebSocket(t, httpServer)
			if !assert.NotNil(t, err) {
				return
			}
			assert.Equal(t, connect.CodeOf(err), test.code)
			if test.mention != "" {
				assert.True(t, strings.Contains(err.Error(), test.mention))
			}
		})
	}
}

// §4.3: a server that echoes no subprotocol has failed the handshake, as one
// that echoes a token the client did not offer has. The client drops the
// connection without a Close frame and reports internal.
func TestHandshakeWithoutSubprotocolEchoIsInternal(t *testing.T) {
	t.Parallel()
	codes := make(chan websocket.CloseError, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(
		func(responseWriter http.ResponseWriter, request *http.Request) {
			// No Subprotocols, so the library echoes none.
			conn, err := websocket.Accept(responseWriter, request, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			_, _, readErr := conn.Read(request.Context())
			codes <- closeErrorOf(readErr)
		},
	))
	t.Cleanup(httpServer.Close)

	err := pingOverWebSocket(t, httpServer)
	if !assert.NotNil(t, err) {
		return
	}
	assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
	assert.True(t, strings.Contains(err.Error(), "echoed no Sec-WebSocket-Protocol"))
	select {
	case closeErr := <-codes:
		// -1: the connection ended without a Close frame.
		assert.Equal(t, closeErr.Code, websocket.StatusCode(-1))
	case <-time.After(10 * time.Second):
		t.Fatal("the client never closed")
	}
}

// §13.1: a connection that ends without S is a failed RPC, reported as
// Unavailable, whether the server closed it cleanly or dropped it without a
// Close frame.
func TestConnectionEndingWithoutEndStreamIsUnavailable(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		end  func(*websocket.Conn) error
	}{
		{name: "close 1000", end: func(conn *websocket.Conn) error {
			return conn.Close(websocket.StatusNormalClosure, "")
		}},
		{name: "no Close frame", end: func(conn *websocket.Conn) error {
			return conn.CloseNow()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := httptest.NewServer(http.HandlerFunc(
				func(responseWriter http.ResponseWriter, request *http.Request) {
					conn, err := websocket.Accept(responseWriter, request, &websocket.AcceptOptions{
						Subprotocols: []string{"connectrpc.1+proto"},
					})
					if err != nil {
						return
					}
					// Wait for the client's M, then answer with M and no S.
					if _, _, err := conn.Read(request.Context()); err != nil {
						_ = conn.CloseNow()
						return
					}
					_ = conn.Write(request.Context(), websocket.MessageText, []byte("M[]"))
					_ = test.end(conn)
				},
			))
			t.Cleanup(httpServer.Close)

			err := pingOverWebSocket(t, httpServer)
			if !assert.NotNil(t, err) {
				return
			}
			assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable)
		})
	}
}
