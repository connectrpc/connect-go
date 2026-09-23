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
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

const trailerKey = "Acme-Trailer"

// trailerServer sets a response trailer on every RPC and can be told to fail.
type trailerServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	fail bool
}

func (s trailerServer) setTrailer(ctx context.Context) {
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		info.ResponseTrailer().Set(trailerKey, "set-by-handler")
	}
}

func (s trailerServer) Ping(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	s.setTrailer(ctx)
	if s.fail {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "handler said no")
	}
	return &pingv1.PingResponse{Number: req.Number}, nil
}

func (s trailerServer) CumSum(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	s.setTrailer(ctx)
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&pingv1.CumSumResponse{Sum: req.Number}); err != nil {
			return err
		}
	}
}

func newTrailerClient(tb testing.TB, handler trailerServer) pingv1connect.PingServiceClient {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, handler)
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)

	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		// Force unary onto WebSocket; the default keeps it on HTTP.
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(tb, err)
	return pingv1connect.NewPingServiceClient(connect.NewClient(transport))
}

// An error the handler returns must reach the caller over WebSocket.
func TestUnaryErrorOverWebSocket(t *testing.T) {
	t.Parallel()
	client := newTrailerClient(t, trailerServer{fail: true})
	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeFailedPrecondition)
}

// Trailers a handler sets must reach the caller on a successful unary call.
func TestUnaryTrailersOverWebSocket(t *testing.T) {
	t.Parallel()
	client := newTrailerClient(t, trailerServer{})
	ctx, info := connect.NewClientContext(t.Context())
	res, err := client.Ping(ctx, &pingv1.PingRequest{Number: 7})
	assert.Nil(t, err)
	assert.Equal(t, res.Number, int64(7))
	assert.Equal(t, info.ResponseTrailer().Get(trailerKey), "set-by-handler")
}

// The streaming case, where the caller reads to io.EOF and so does consume the
// end-of-stream message.
func TestStreamingTrailersOverWebSocket(t *testing.T) {
	t.Parallel()
	client := newTrailerClient(t, trailerServer{})
	ctx, info := connect.NewClientContext(t.Context())
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 3}))
	_, err = stream.Receive()
	assert.Nil(t, err)
	assert.Nil(t, stream.CloseSend())
	_, err = stream.Receive()
	assert.True(t, errors.Is(err, io.EOF))
	assert.Nil(t, stream.Close())
	assert.Equal(t, info.ResponseTrailer().Get(trailerKey), "set-by-handler")
}
