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
	"log"
	"os"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

func Example_client() {
	logger := log.New(os.Stdout, "" /* prefix */, 0 /* flags */)
	// To keep this example runnable, we'll use an in-memory server and client.
	// By default, transports use the Connect protocol. Add connect.WithGRPC() or
	// connect.WithGRPCWeb() to switch protocols.
	connectClient := connect.NewClient(connecthttp.NewTransport(examplePingServer.Client(), examplePingServer.URL()))

	client := pingv1connect.NewPingServiceClient(connectClient)
	response, err := client.Ping(
		context.Background(),
		&pingv1.PingRequest{Number: 42},
	)
	if err != nil {
		logger.Println("error:", err)
		return
	}
	logger.Println("response:", response)

	// Output:
	// response: number:42
}
