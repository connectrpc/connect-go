// Copyright 2021-2023 Buf Technologies, Inc.
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
	"fmt"
	"net/http"

	"github.com/bufbuild/connect-go"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
	"github.com/bufbuild/connect-go/internal/gen/connect/ping/v1/pingv1connect"
)

// ExampleCachingServer is an example of how servers can take advantage the
// Connect protocol's support for HTTP-level caching. The Protobuf
// definition for this API is in proto/connect/ping/v1/ping.proto.
type ExampleCachingPingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

// Ping is idempotent and free of side effects (and the Protobuf schema
// indicates this), so clients using the Connect protocol may call it with HTTP
// GET requests. This implementation uses Etags to manage client-side caching.
func (*ExampleCachingPingServer) Ping(
	_ context.Context,
	req *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {
	resp := connect.NewResponse(&pingv1.PingResponse{
		Number: req.Msg.Number,
	})
	// Our hashing logic is simple: we use the number in the PingResponse.
	hash := fmt.Sprint(resp.Msg.Number)
	// If the request was an HTTP GET (which always has URL query parameters),
	// we'll need to check if the client already has the response cached.
	if len(req.Peer().Query) > 0 {
		if req.Header().Get("If-None-Match") == hash {
			return nil, connect.NewNotModifiedError(http.Header{
				"Etag": []string{hash},
			})
		}
		resp.Header().Set("Etag", hash)
	}
	return resp, nil
}

func ExampleNewNotModifiedError() {
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(&ExampleCachingPingServer{}))
	_ = http.ListenAndServe("localhost:8080", mux)
}
