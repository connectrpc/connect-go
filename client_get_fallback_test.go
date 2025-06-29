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

package connect

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/memhttp"
	"connectrpc.com/connect/internal/memhttp/memhttptest"
)

func TestClientUnaryGetFallback(t *testing.T) {
	t.Parallel()
	// Importing pingv1connect to get the procedure name results in cyclic dependencies in this file
	pingProcedure := "/connect.ping.v1.PingService/Ping"

	newClient := func(server *memhttp.Server) *Client[pingv1.PingRequest, pingv1.PingResponse] {
		return NewClient[pingv1.PingRequest, pingv1.PingResponse](
			server.Client(),
			server.URL()+pingProcedure,
			WithHTTPGet(),
			WithHTTPGetMaxURLSize(1, true),
			WithSendGzip(),
		)
	}

	t.Run("generics_api", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.Handle(pingProcedure, NewUnaryHandler(
			pingProcedure,
			func(ctx context.Context, r *Request[pingv1.PingRequest]) (*Response[pingv1.PingResponse], error) {
				return NewResponse(&pingv1.PingResponse{
					Number: r.Msg.GetNumber(),
					Text:   r.Msg.GetText(),
				}), nil
			},
			WithIdempotency(IdempotencyNoSideEffects),
		))
		server := memhttptest.NewServer(t, mux)
		client := newClient(server)
		ctx := context.Background()

		_, err := client.CallUnary(ctx, NewRequest[pingv1.PingRequest](nil))
		assert.Nil(t, err)

		text := strings.Repeat(".", 256)
		r, err := client.CallUnary(ctx, NewRequest(&pingv1.PingRequest{Text: text}))
		assert.Nil(t, err)
		assert.Equal(t, r.Msg.GetText(), text)
	})
	t.Run("simple_api", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.Handle(pingProcedure, NewUnaryHandlerSimple(
			pingProcedure,
			func(ctx context.Context, r *pingv1.PingRequest) (*pingv1.PingResponse, error) {
				return &pingv1.PingResponse{
					Number: r.GetNumber(),
					Text:   r.GetText(),
				}, nil
			},
			WithIdempotency(IdempotencyNoSideEffects),
		))
		server := memhttptest.NewServer(t, mux)
		client := newClient(server)
		ctx := context.Background()

		_, err := client.CallUnarySimple(ctx, &pingv1.PingRequest{})
		assert.Nil(t, err)

		text := strings.Repeat(".", 256)
		r, err := client.CallUnarySimple(ctx, &pingv1.PingRequest{Text: text})
		assert.Nil(t, err)
		assert.Equal(t, r.GetText(), text)
	})
}
