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
	"fmt"
	"net/http"

	connect "connectrpc.com/connect"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/generics/connect/ping/v1/pingv1connect"
)

func ExampleError_Message() {
	err := fmt.Errorf(
		"another: %w",
		connect.NewError(connect.CodeUnavailable, errors.New("failed to foo")),
	)
	if connectErr := (&connect.Error{}); errors.As(err, &connectErr) {
		fmt.Println("underlying error message:", connectErr.Message())
	}

	// Output:
	// underlying error message: failed to foo
}

func ExampleIsNotModifiedError() {
	// Assume that the server from NewNotModifiedError's example is running on
	// localhost:8080.
	client := pingv1connect.NewPingServiceClient(
		http.DefaultClient,
		"http://localhost:8080",
		// Enable client-side support for HTTP GETs.
		connect.WithHTTPGet(),
	)
	req := connect.NewRequest(&pingv1.PingRequest{Number: 42})
	first, err := client.Ping(context.Background(), req)
	if err != nil {
		fmt.Println(err)
		return
	}
	// If the server set an Etag, we can use it to cache the response.
	etag := first.Header().Get("Etag")
	if etag == "" {
		fmt.Println("no Etag in response headers")
		return
	}
	fmt.Println("cached response with Etag", etag)
	// Now we'd like to make the same request again, but avoid re-fetching the
	// response if possible.
	req.Header().Set("If-None-Match", etag)
	_, err = client.Ping(context.Background(), req)
	if connect.IsNotModifiedError(err) {
		fmt.Println("can reuse cached response")
	}
}
