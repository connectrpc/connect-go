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

package main

import (
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// newTestClient builds a client that dispatches directly to the server in the
// same process, with no listener and no serialization.
func newTestClient(tb testing.TB) pingv1connect.PingServiceClient {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	return pingv1connect.NewPingServiceClient(
		connect.NewClient(connectinprocess.New(server)),
	)
}

func TestPing(t *testing.T) {
	client := newTestClient(t)
	res, err := client.Ping(t.Context(), &v1.PingRequest{Number: 42, Text: "hello"})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if res.Number != 42 || res.Text != "hello" {
		t.Errorf("got Number=%d Text=%q; want 42 %q", res.Number, res.Text, "hello")
	}
}
