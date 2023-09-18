// Copyright 2021-2023 The Connect Authors
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
	"net/http"
	"net/http/httptest"

	"connectrpc.com/connect/internal/connecttest"
	"connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect"
)

var examplePingServer *httptest.Server

func init() {
	// Generally, init functions are bad. However, we need to set up the server
	// before the examples run.
	//
	// To write testable examples that users can grok *and* can execute in the
	// playground we use an in memory pipe as network based playgrounds can
	// deadlock, see:
	// (https://github.com/golang/go/issues/48394)
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{}))
	examplePingServer = newInMemoryServer(mux)
}

// newInMemoryServer constructs and starts an inMemoryServer.
func newInMemoryServer(handler http.Handler) *httptest.Server {
	lis := connecttest.NewMemoryListener()
	server := httptest.NewUnstartedServer(handler)
	server.Listener = lis
	server.EnableHTTP2 = true
	server.StartTLS()
	client := server.Client()
	// Configure the httptest.Server client to use the in-memory listener.
	// Automatic HTTP-level gzip  compression is disabled.
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.DialContext = lis.DialContext
		transport.DisableCompression = true
	}
	return server
}
