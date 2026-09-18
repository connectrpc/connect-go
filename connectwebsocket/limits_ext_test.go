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
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// echoSizeServer answers with a response of a fixed size, so a test can put
// the limit on the client's receiving side.
type echoSizeServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	responseBytes int
}

func (s echoSizeServer) Ping(_ context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	return &pingv1.PingResponse{
		Number: req.Number,
		Text:   strings.Repeat("x", s.responseBytes),
	}, nil
}

// SendMaxBytes has to bite before the message reaches the socket, whichever
// transport carries it.
func TestSendMaxBytesRejectsOversizeRequest(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, echoSizeServer{})
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
		connectwebsocket.WithSendMaxBytes(128),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
	assert.Nil(t, err)

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{Text: strings.Repeat("x", 4096)})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
	assert.True(t, strings.Contains(err.Error(), "sendMaxBytes"))
}

// The receiving half of the same limit: a response the client never asked to
// be this large must be refused rather than buffered.
func TestReadMaxBytesRejectsOversizeResponse(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, echoSizeServer{responseBytes: 64 * 1024})
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
		connectwebsocket.WithReadMaxBytes(1024),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
}

// A JSON round trip over WebSocket. The subprotocol test pins the token; this
// pins that a message actually survives the codec it names.
func TestJSONCodecRoundTrip(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, pingServer{})
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSendCodec(connect.CodecNameJSON),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	t.Cleanup(func() { _ = stream.Close() })
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 5}))
	res, err := stream.Receive()
	assert.Nil(t, err)
	assert.Equal(t, res.Sum, int64(5))
}
