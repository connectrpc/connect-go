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

package connect_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	connect "connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/generics/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/internal/memhttp/memhttptest"
)

func TestProxyPreservesErrorDetailDebug(t *testing.T) {
	t.Parallel()
	// A proxy without descriptors for a detail's type forwards the detail's
	// human-readable debug JSON unchanged.
	const detail = `{"type":"acme.user.v1.User","value":"AQID","debug":{"name":"alice"}}`
	upstream := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":"invalid_argument","message":"bad","details":[`+detail+`]}`)
	}))
	upstreamClient := pingv1connect.NewPingServiceClient(upstream.Client(), upstream.URL())
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.PingServicePingProcedure, connect.NewUnaryHandler(
		pingv1connect.PingServicePingProcedure,
		func(ctx context.Context, req *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
			return upstreamClient.Ping(ctx, req)
		},
	))
	proxy := memhttptest.NewServer(t, mux)
	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		proxy.URL()+pingv1connect.PingServicePingProcedure,
		strings.NewReader("{}"),
	)
	assert.Nil(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := proxy.Client().Do(request)
	assert.Nil(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	assert.Nil(t, err)
	assert.True(t, strings.Contains(string(body), `"debug":{"name":"alice"}`), assert.Sprintf("body: %s", body))
}

func TestClientRequestsExactURL(t *testing.T) {
	t.Parallel()
	// The client sends to the URL it was given, even when the path has a
	// prefix or a trailing slash.
	paths := make(chan string, 1)
	server := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.WriteHeader(http.StatusNotFound)
	}))
	for _, path := range []string{
		"/prefix/connect.ping.v1.PingService/Ping",
		"/connect.ping.v1.PingService/Ping/",
	} {
		client := connect.NewClient[pingv1.PingRequest, pingv1.PingResponse](server.Client(), server.URL()+path)
		_, err := client.CallUnary(t.Context(), connect.NewRequest(&pingv1.PingRequest{}))
		assert.NotNil(t, err)
		assert.Equal(t, <-paths, path)
	}
}
