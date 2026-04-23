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
	"net/http"

	"connectrpc.com/connect/internal/memhttp"
)

// HandlerClient is an [HTTPClient] that dispatches requests directly to an
// [http.Handler] over an in-memory transport. It enables composing Connect
// services in the same process without a network while preserving full
// protocol fidelity for Connect, gRPC, and gRPC-Web, including full-duplex
// streaming.
//
// HandlerClient is safe for concurrent use. Each HandlerClient owns a
// background goroutine that services the in-memory transport for its
// lifetime. Long-lived HandlerClients held for the lifetime of the process
// do not require cleanup; callers that create and discard HandlerClients
// at runtime should Close them to release that goroutine.
type HandlerClient struct {
	server *memhttp.Server
}

// NewHandlerClient returns a [HandlerClient] that dispatches requests to
// handler. Pass the returned client and its URL to [NewClient] or a
// generated service constructor:
//
//	c := connect.NewHandlerClient(handler)
//	client := pingv1connect.NewPingServiceClient(c, c.URL())
func NewHandlerClient(handler http.Handler) *HandlerClient {
	return &HandlerClient{server: memhttp.NewServer(handler)}
}

// Do implements [HTTPClient].
func (c *HandlerClient) Do(req *http.Request) (*http.Response, error) {
	return c.server.Client().Do(req) //nolint:gosec // in-memory transport, no external network
}

// URL returns a syntactically valid http:// URL for c's handler. It is not
// bound to a network address.
func (c *HandlerClient) URL() string {
	return c.server.URL()
}

// Close releases the background resources used by c. It stops the
// accept-loop goroutine, closes the in-memory listener, and terminates any
// active connections. Close is idempotent.
func (c *HandlerClient) Close() error {
	return c.server.Close()
}
