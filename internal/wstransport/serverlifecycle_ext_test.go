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
	"context"
	"encoding/json"
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

// §13: a server shutting down mid-RPC sends S with unavailable and closes with
// 1001, though http.Server.Shutdown cannot see a hijacked connection.
func TestShutdownEndsStreamsWithGoingAway(t *testing.T) {
	t.Parallel()
	httpServer := newMountedServer(t)
	conn := dialCumSum(t, httpServer, "")
	sendProtoBody(t, conn, &pingv1.CumSumRequest{Number: 1})
	data := readServerOpening(t, conn)
	assert.Equal(t, data[0], wireBody)

	ctx, cancel := context.WithTimeout(t.Context(), wsTestReadTimeout)
	defer cancel()
	assert.Nil(t, httpServer.Config.Shutdown(ctx))

	readCtx, cancelRead := readContext(t)
	defer cancelRead()
	_, endStream, err := conn.Read(readCtx)
	assert.Nil(t, err)
	assert.Equal(t, endStream[0], wireServerEndStream)
	assert.True(t, strings.Contains(string(endStream), `"code":"unavailable"`), assert.Sprintf("%s", endStream))
	assert.Equal(t, readCloseStatus(t, conn), websocket.StatusGoingAway)
}

// A client that upgrades and never sends its opening M is hung up on after the
// handshake timeout, not after the RPC's hour-long default.
func TestClientThatNeverOpensTheStreamIsHungUpOn(t *testing.T) {
	t.Parallel()
	httpServer := newMountedServer(t, connecthttp.WithWebSocketHandshakeTimeout(200*time.Millisecond))
	conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)

	data := readServerOpening(t, conn)
	assert.Equal(t, data[0], wireServerEndStream)
	assert.True(t, strings.Contains(string(data), `"code":"deadline_exceeded"`), assert.Sprintf("%s", data))
	assert.True(t, strings.Contains(string(data), "opening M"))
	assert.Equal(t, readCloseStatus(t, conn), websocket.StatusNormalClosure)
}

// §9 and §7.3.1: a trailer the server may not send is not sent, and it becomes
// the RPC's outcome, as an unsendable header does.
func TestServerRefusesToSendInvalidTrailer(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct{ key, value string }{
		"control character": {key: "Acme-Bad", value: "a\nb"},
		"binary not base64": {key: "Acme-Bad-Bin", value: "not base64!"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			httpServer := newServerFor(t, trailerWritingServer{key: test.key, value: test.value})
			conn := dialCumSum(t, httpServer, "")
			sendJSONMessage(t, conn, wireClientEndStream, nil)

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			var end struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
				Metadata map[string][]string `json:"metadata"`
			}
			assert.Nil(t, json.Unmarshal(data[1:], &end))
			assert.Equal(t, end.Error.Code, "internal")
			assert.True(t, strings.Contains(end.Error.Message, test.key), assert.Sprintf("%s", end.Error.Message))
			assert.Equal(t, len(end.Metadata), 0)
		})
	}
}

// unwrappingWriter is middleware's response writer: it hides the Hijacker it
// wraps, and exposes it through Unwrap as http.ResponseController expects.
type unwrappingWriter struct {
	responseWriter http.ResponseWriter
}

func (w *unwrappingWriter) Header() http.Header            { return w.responseWriter.Header() }
func (w *unwrappingWriter) Write(data []byte) (int, error) { return w.responseWriter.Write(data) }
func (w *unwrappingWriter) WriteHeader(statusCode int)     { w.responseWriter.WriteHeader(statusCode) }
func (w *unwrappingWriter) Unwrap() http.ResponseWriter    { return w.responseWriter }

// Middleware that wraps the response writer does not break the upgrade.
func TestUpgradeFindsTheHijackerThroughUnwrap(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mux.ServeHTTP(&unwrappingWriter{responseWriter: writer}, request)
	}))
	t.Cleanup(httpServer.Close)
	client := newWebSocketClient(t, httpServer.Client(), httpServer.URL)

	// A failed hijack leaves the client waiting, so bound it.
	deadlineCtx, cancel := context.WithTimeout(t.Context(), wsTestReadTimeout)
	defer cancel()
	ctx, info := connect.NewClientContext(deadlineCtx)
	response, err := client.Ping(ctx, &pingv1.PingRequest{Number: 3})
	assert.Nil(t, err)
	assert.Equal(t, response.GetNumber(), int64(3))
	assert.Equal(t, info.Protocol, connecthttp.ProtocolNameConnectWebSocket)
}
