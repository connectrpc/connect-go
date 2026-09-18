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

package connectwebsocket_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// countUpServer answers the server-streaming procedure, which is the one that
// works over either transport: bidi needs HTTP/2, but a server stream is
// ordinary chunked HTTP.
type countUpServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (countUpServer) Ping(_ context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	return &pingv1.PingResponse{Number: req.Number}, nil
}

func (countUpServer) CountUp(
	_ context.Context,
	req *pingv1.CountUpRequest,
	stream pingv1connect.PingServiceCountUpServerStream,
) error {
	for i := int64(1); i <= req.Number; i++ {
		if err := stream.Send(&pingv1.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
	return nil
}

func (countUpServer) CumSum(_ context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	var total int64
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		total += req.Number
		if err := stream.Send(&pingv1.CumSumResponse{Sum: total}); err != nil {
			return err
		}
	}
}

// newSelectorServer mounts one server for every selector test: both transports
// on every path, so the client is free to choose.
func newSelectorServer(tb testing.TB) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, countUpServer{})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// The selector is the client's alone, and the same server answers whichever
// way it chooses. Each case runs a unary call and a server stream against one
// server and records the protocol that actually carried them.
func TestSelectorChoosesTheWirePerRPC(t *testing.T) {
	t.Parallel()
	const websocketProtocol = connectwebsocket.ProtocolConnectWebSocket
	httpProtocol := connect.ProtocolNameConnect

	for _, test := range []struct {
		name          string
		selector      connectwebsocket.Selector
		wantUnary     string
		wantStreaming string
	}{
		{
			name:          "default routes streaming to WebSocket",
			wantUnary:     httpProtocol,
			wantStreaming: websocketProtocol,
		},
		{
			name:          "SelectAll routes everything to WebSocket",
			selector:      connectwebsocket.SelectAll,
			wantUnary:     websocketProtocol,
			wantStreaming: websocketProtocol,
		},
		{
			name: "SelectBidi leaves server streams on HTTP",
			// Only full-duplex RPCs need the upgrade; a server stream is
			// ordinary chunked HTTP and works without it.
			selector:      connectwebsocket.SelectBidi,
			wantUnary:     httpProtocol,
			wantStreaming: httpProtocol,
		},
		{
			name: "a custom selector routes one procedure",
			selector: func(spec connect.Spec) bool {
				return strings.HasSuffix(spec.Procedure, "/CountUp")
			},
			wantUnary:     httpProtocol,
			wantStreaming: websocketProtocol,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newSelectorServer(t)
			options := []connectwebsocket.ClientOption{
				connectwebsocket.WithHTTPClient(httpServer.Client()),
			}
			if test.selector != nil {
				options = append(options, connectwebsocket.WithSelector(test.selector))
			}
			transport, err := connectwebsocket.NewTransport(httpServer.URL, options...)
			assert.Nil(t, err)

			var protocol string
			client := pingv1connect.NewPingServiceClient(
				connect.NewClient(transport, recordProtocol(&protocol)),
			)

			response, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 7})
			assert.Nil(t, err)
			assert.Equal(t, response.Number, int64(7))
			assert.Equal(t, protocol, test.wantUnary)

			stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 3})
			assert.Nil(t, err)
			var got []int64
			for {
				message, err := stream.Receive()
				if err != nil {
					break
				}
				got = append(got, message.Number)
			}
			assert.Nil(t, stream.Close())
			assert.Equal(t, got, []int64{1, 2, 3})
			assert.Equal(t, protocol, test.wantStreaming)
		})
	}
}

// Two clients with opposite choices, against one server, at the same time: the
// point of registering both transports on every path.
func TestOneServerServesBothChoicesAtOnce(t *testing.T) {
	t.Parallel()
	httpServer := newSelectorServer(t)

	newClient := func(selector connectwebsocket.Selector, protocol *string) pingv1connect.PingServiceClient {
		transport, err := connectwebsocket.NewTransport(
			httpServer.URL,
			connectwebsocket.WithHTTPClient(httpServer.Client()),
			connectwebsocket.WithSelector(selector),
		)
		assert.Nil(t, err)
		return pingv1connect.NewPingServiceClient(
			connect.NewClient(transport, recordProtocol(protocol)),
		)
	}

	var overWebSocket, overHTTP string
	websocketClient := newClient(connectwebsocket.SelectAll, &overWebSocket)
	httpClient := newClient(func(connect.Spec) bool { return false }, &overHTTP)

	for _, client := range []pingv1connect.PingServiceClient{websocketClient, httpClient} {
		stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 2})
		assert.Nil(t, err)
		var got []int64
		for {
			message, err := stream.Receive()
			if err != nil {
				break
			}
			got = append(got, message.Number)
		}
		assert.Nil(t, stream.Close())
		assert.Equal(t, got, []int64{1, 2})
	}

	assert.Equal(t, overWebSocket, connectwebsocket.ProtocolConnectWebSocket)
	assert.Equal(t, overHTTP, connect.ProtocolNameConnect)
}
