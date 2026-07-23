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

package connectinprocess_test

import (
	"errors"
	"io"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

func BenchmarkInProcessUnary(b *testing.B) {
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, pingServer{})
	transport := connectinprocess.New(handler)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	req := &pingv1.PingRequest{Number: 42, Text: "hello"}
	ctx := b.Context()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := client.Ping(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkInProcessServerStreaming(b *testing.B) {
	const messages = 8
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, pingServer{})
	transport := connectinprocess.New(handler)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	ctx := b.Context()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: messages})
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, err := stream.Receive(); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				b.Fatal(err)
			}
		}
	}
}
