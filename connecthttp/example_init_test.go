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

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/memhttp"
)

var examplePingServer *memhttp.Server

func init() {
	// Generally, init functions are bad. However, we need to set up the server
	// before the examples run.
	//
	// To write testable examples that users can grok *and* can execute in the
	// playground we use an in memory pipe as network based playgrounds can
	// deadlock, see:
	// (https://github.com/golang/go/issues/48394)
	mux := http.NewServeMux()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	connecthttp.Mount(mux, server)
	examplePingServer = memhttp.NewServer(mux)
}
