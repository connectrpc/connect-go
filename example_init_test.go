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

package connect_test

import (
	"net/http"

	"connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect"
	pingv1connectsimple "connectrpc.com/connect/internal/gen/simple/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/internal/memhttp"
)

var examplePingServer *memhttp.Server
var examplePingServerSimple *memhttp.Server

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
	examplePingServer = memhttp.NewServer(mux)

	muxSimple := http.NewServeMux()
	muxSimple.Handle(pingv1connectsimple.NewPingServiceHandler(pingServerSimple{}))
	examplePingServerSimple = memhttp.NewServer(muxSimple)
}
