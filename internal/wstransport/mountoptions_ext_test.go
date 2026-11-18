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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

func newWebSocketClient(tb testing.TB, httpClient connecthttp.HTTPClient, baseURL string) pingv1connect.PingServiceClient {
	tb.Helper()
	transport := connecthttp.NewTransport(httpClient, baseURL, connecthttp.WithWebSocket(connecthttp.SelectAll))
	return pingv1connect.NewPingServiceClient(connect.NewClient(transport))
}

// Per-procedure options configure a procedure whichever transport carries it.
func TestConditionalOptionsReachWebSocketRoutes(t *testing.T) {
	t.Parallel()
	httpServer := newMountedServer(t, connecthttp.WithConditionalOptions(func(spec connect.Spec) []connecthttp.Option {
		if spec.Procedure == pingv1connect.PingServiceCumSumProcedure {
			return []connecthttp.Option{connecthttp.WithReadMaxBytes(1)}
		}
		return nil
	}))
	client := newWebSocketClient(t, httpServer.Client(), httpServer.URL)

	ctx, info := connect.NewClientContext(t.Context())
	_, err := client.Ping(ctx, &pingv1.PingRequest{Text: strings.Repeat("x", 64)})
	assert.Nil(t, err)
	assert.Equal(t, info.Protocol, connecthttp.ProtocolNameConnectWebSocket)

	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1000}))
	_, err = stream.Receive()
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
	assert.Nil(t, stream.Close())
}

func TestConditionalOptionsReachWebSocketClients(t *testing.T) {
	t.Parallel()
	httpServer := newMountedServer(t)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithConditionalOptions(func(spec connect.Spec) []connecthttp.Option {
			if spec.Procedure == pingv1connect.PingServicePingProcedure {
				return []connecthttp.Option{connecthttp.WithWebSocket(connecthttp.SelectAll)}
			}
			return nil
		}),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	ctx, info := connect.NewClientContext(t.Context())
	_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 1})
	assert.Nil(t, err)
	assert.Equal(t, info.Protocol, connecthttp.ProtocolNameConnectWebSocket)

	ctx, info = connect.NewClientContext(t.Context())
	_, err = client.Fail(ctx, &pingv1.FailRequest{})
	assert.NotNil(t, err)
	assert.Equal(t, info.Protocol, connect.ProtocolNameConnect)
}

// prefixMux serves every route under a prefix it does not strip, as a router
// mounting a sub-router might.
type prefixMux struct {
	mux    *http.ServeMux
	prefix string
}

func (m prefixMux) Handle(pattern string, handler http.Handler) {
	m.mux.Handle(m.prefix+pattern, handler)
}

// A route registered for a procedure serves that procedure, whatever path the
// mux reached it by, as it does over HTTP.
func TestWebSocketRouteServesItsRegisteredProcedure(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(prefixMux{mux: mux, prefix: "/api"}, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	client := newWebSocketClient(t, httpServer.Client(), httpServer.URL+"/api")

	response, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 7})
	assert.Nil(t, err)
	assert.Equal(t, response.GetNumber(), int64(7))
}

type serverInfoServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	infos chan *connecthttp.ServerInfo
	hosts chan string
}

func (s serverInfoServer) Ping(ctx context.Context, _ *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	info, _ := connecthttp.ServerInfoForContext(ctx)
	s.infos <- info
	callInfo, _ := connect.CallInfoForServerContext(ctx)
	s.hosts <- callInfo.RequestHeader().Get("Host")
	return &pingv1.PingResponse{}, nil
}

// A handler reads the same transport details whichever transport ran it.
func TestWebSocketRPCCarriesServerInfo(t *testing.T) {
	t.Parallel()
	handler := serverInfoServer{infos: make(chan *connecthttp.ServerInfo, 1), hosts: make(chan string, 1)}
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, handler)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	client := newWebSocketClient(t, httpServer.Client(), httpServer.URL)

	_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
	assert.Nil(t, err)
	info := <-handler.infos
	assert.NotNil(t, info)
	assert.Equal(t, info.HTTPMethod(), http.MethodGet)
	requestURL := info.RequestURL()
	assert.Equal(t, requestURL.Path, pingv1connect.PingServicePingProcedure)
	assert.Equal(t, <-handler.hosts, strings.TrimPrefix(httpServer.URL, "http://"))
}

type countingHTTPClient struct {
	client *http.Client
	calls  atomic.Int32
}

func (c *countingHTTPClient) Do(request *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.client.Do(request)
}

// An HTTPClient that is not an *http.Client still carries the handshake, so
// whatever it adds (auth, tracing, a proxy) applies to both transports.
func TestHandshakeGoesThroughACustomHTTPClient(t *testing.T) {
	t.Parallel()
	httpServer := newMountedServer(t)
	httpClient := &countingHTTPClient{client: httpServer.Client()}
	client := newWebSocketClient(t, httpClient, httpServer.URL)

	ctx, info := connect.NewClientContext(t.Context())
	_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 1})
	assert.Nil(t, err)
	assert.Equal(t, info.Protocol, connecthttp.ProtocolNameConnectWebSocket)
	assert.Equal(t, httpClient.calls.Load(), int32(1))
}

type requestHeaderServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	seen chan string
}

func (s requestHeaderServer) CumSum(ctx context.Context, _ pingv1connect.PingServiceCumSumServerStream) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	s.seen <- info.RequestHeader().Get("Acme-Late")
	return nil
}

// Request headers may be set until the first Send, as over HTTP.
func TestRequestHeadersAreReadAtFirstSend(t *testing.T) {
	t.Parallel()
	handler := requestHeaderServer{seen: make(chan string, 1)}
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, handler)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	client := newWebSocketClient(t, httpServer.Client(), httpServer.URL)

	ctx, info := connect.NewClientContext(t.Context())
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	info.RequestHeader().Set("Acme-Late", "set after the stream opened")
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	assert.Equal(t, <-handler.seen, "set after the stream opened")
	assert.Nil(t, stream.CloseSend())
	_, err = stream.Receive()
	assert.True(t, errors.Is(err, io.EOF))
	assert.Nil(t, stream.Close())
}

// The client's CallInfo is as complete as over HTTP: the spec, the message
// sizes, and the HTTP exchange, which for WebSocket is the handshake.
func TestClientCallInfoDescribesTheCall(t *testing.T) {
	t.Parallel()
	httpServer := newMountedServer(t)
	client := newWebSocketClient(t, httpServer.Client(), httpServer.URL)

	ctx, info := connect.NewClientContext(t.Context())
	_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 1, Text: "hello"})
	assert.Nil(t, err)
	assert.Equal(t, info.Spec.Procedure, pingv1connect.PingServicePingProcedure)
	assert.Equal(t, info.Spec.StreamType, connect.StreamTypeUnary)
	assert.True(t, info.SendStats.Size > 0)
	assert.True(t, info.ReceiveStats.Size > 0)
	clientInfo, ok := connecthttp.ClientInfoForContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, clientInfo.HTTPMethod(), http.MethodGet)
	assert.Equal(t, clientInfo.ResponseStatus(), http.StatusSwitchingProtocols)
}
