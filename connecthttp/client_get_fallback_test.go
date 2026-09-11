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

package connecthttp_test

import (
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/memhttp/memhttptest"
)

func TestClientUnaryGetFallback(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, pingServer{})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)

	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(),
		server.URL(),
		connecthttp.WithHTTPGet(),
		connecthttp.WithHTTPGetMaxURLSize(1, true),
		connecthttp.WithSendCompression("gzip"),
	)))
	ctx := t.Context()

	_, err := client.Ping(ctx, &pingv1.PingRequest{})
	assert.Nil(t, err)

	text := strings.Repeat(".", 256)
	r, err := client.Ping(ctx, &pingv1.PingRequest{Text: text})
	assert.Nil(t, err)
	assert.Equal(t, r.GetText(), text)
}
