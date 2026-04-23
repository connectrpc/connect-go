// Copyright 2021-2025 The Connect Authors
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
	"net"
	"net/http"

	"golang.org/x/net/http2"
)

// inProcessURL is the placeholder URL returned alongside the in-process
// [HTTPClient]. The host matches httptest.DefaultRemoteAddr.
const inProcessURL = "http://1.2.3.4"

// NewInProcessHTTPClient returns an [HTTPClient] that dispatches requests
// directly to handler, along with a URL to pair with it. It supports all
// protocols and stream types.
//
// Pass both return values to a generated service constructor:
//
//	client := pingv1connect.NewPingServiceClient(
//		connect.NewInProcessHTTPClient(mux),
//	)
//
// or unpack them to pass additional options:
//
//	hc, url := connect.NewInProcessHTTPClient(mux)
//	client := pingv1connect.NewPingServiceClient(hc, url, connect.WithGRPC())
func NewInProcessHTTPClient(handler http.Handler) (HTTPClient, string) {
	http2Server := &http2.Server{}
	transport := &http.Transport{
		// DisableKeepAlives forces one connection per request, so each
		// request's lifetime is also the lifetime of its ServeConn
		// goroutine. No idle pool, no teardown to manage.
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			// TODO: implement buffered pipe to prevent deadlocks.
			serverConn, clientConn := net.Pipe()
			// The server-side base context stays at Background so caller
			// values don't leak into the handler. Per-request cancellation
			// still propagates: cancelling the caller's context triggers
			// HTTP/2 RST_STREAM, which cancels the handler's req.Context().
			go http2Server.ServeConn(serverConn, &http2.ServeConnOpts{
				Handler: handler,
				Context: context.Background(),
			})
			return clientConn, nil
		},
	}
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport.Protocols = protocols
	return &http.Client{Transport: transport}, inProcessURL
}
