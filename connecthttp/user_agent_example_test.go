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
	"context"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// userAgentRecorder wraps an HTTP client and prints the User-Agent header of
// each request, so the example can show what goes over the wire.
type userAgentRecorder struct {
	connecthttp.HTTPClient
}

func (r userAgentRecorder) Do(request *http.Request) (*http.Response, error) {
	values, present := request.Header["User-Agent"]
	switch {
	case !present:
		fmt.Println("User-Agent: <not set>")
	case strings.HasPrefix(values[0], "connect-go/"):
		fmt.Println("User-Agent: <connect default>")
	default:
		fmt.Printf("User-Agent: %q\n", values[0])
	}
	return r.HTTPClient.Do(request)
}

// This example shows how to share one application-wide User-Agent across
// several clients. The interceptor only fills in the header when the call
// doesn't already carry one, so a single request can still override the shared
// value, including with an explicitly empty value. This only affects the
// standard HTTP User-Agent header; the gRPC-Web X-User-Agent header is
// separate.
func Example_clientUserAgent() {
	const appUserAgent = "my-app/1.2.3"
	defaultUserAgent := func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				if len(info.RequestHeader().Values("User-Agent")) == 0 {
					info.RequestHeader().Set("User-Agent", appUserAgent)
				}
			}
			return next(ctx, spec)
		}
	}
	httpClient := userAgentRecorder{examplePingServer.Client()}
	// Both clients share the same interceptor and HTTP client.
	pingClient := pingv1connect.NewPingServiceClient(
		connect.NewClient(connecthttp.NewTransport(httpClient, examplePingServer.URL()), defaultUserAgent),
	)
	otherPingClient := pingv1connect.NewPingServiceClient(
		connect.NewClient(connecthttp.NewTransport(httpClient, examplePingServer.URL()), defaultUserAgent),
	)

	ping := func(ctx context.Context, client pingv1connect.PingServiceClient) {
		if _, err := client.Ping(ctx, &pingv1.PingRequest{Number: 42}); err != nil {
			fmt.Println("error:", err)
		}
	}

	// The shared default is applied to both clients.
	ping(context.Background(), pingClient)
	ping(context.Background(), otherPingClient)

	// A request can provide its own User-Agent...
	ctx, info := connect.NewClientContext(context.Background())
	info.RequestHeader().Set("User-Agent", "my-app/1.2.3 batch-job")
	ping(ctx, pingClient)

	// ...or set an explicitly empty value, which is left alone.
	ctx, info = connect.NewClientContext(context.Background())
	info.RequestHeader().Set("User-Agent", "")
	ping(ctx, pingClient)

	// Output:
	// User-Agent: "my-app/1.2.3"
	// User-Agent: "my-app/1.2.3"
	// User-Agent: "my-app/1.2.3 batch-job"
	// User-Agent: ""
}
