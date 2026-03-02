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
	"context"
	"errors"
	"net/http"
	"strconv"

	connect "connectrpc.com/connect"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/simple/connect/ping/v1/pingv1connect"
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
	ctx context.Context,
	req *pingv1.PingRequest,
) (*pingv1.PingResponse, error) {
	resp := &pingv1.PingResponse{
		Number: req.GetNumber(),
	}
	callInfo, ok := connect.CallInfoForHandlerContext(ctx)
	if !ok {
		return nil, errors.New("no call info found in context")
	}

	// Our hashing logic is simple: we use the number in the PingResponse.
	hash := strconv.FormatInt(resp.GetNumber(), 10)
	// If the request was an HTTP GET, we'll need to check if the client already
	// has the response cached.
	if callInfo.HTTPMethod() == http.MethodGet && callInfo.RequestHeader().Get("If-None-Match") == hash {
		return nil, connect.NewNotModifiedError(http.Header{
			"Etag": []string{hash},
		})
	}
	callInfo.ResponseHeader().Set("Etag", hash)
	return resp, nil
}

func ExampleNewNotModifiedError() {
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(&ExampleCachingPingServer{}))
	_ = http.ListenAndServe("localhost:8080", mux)
}
