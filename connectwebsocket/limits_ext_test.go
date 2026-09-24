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
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// A server that rejects an oversized request explains itself in an S message
// rather than hanging up with a bare 1009. Asserted through IsRemote: a close
// status the client mapped locally would carry the same code, and only the
// origin tells the two apart.
func TestOversizeRequestIsExplainedInBand(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, echoSizeServer{})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server, connectwebsocket.WithReadMaxBytes(1024))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{Text: strings.Repeat("x", 8192)})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)

	var connectErr *connect.Error
	assert.True(t, errors.As(err, &connectErr))
	assert.True(t, connectErr.IsRemote())
	// The configured limit, not the wire limit that carries the marker margin.
	assert.True(t, strings.Contains(err.Error(), "configured max of 1024 bytes"))
}

// The read limit bounds a whole WebSocket message, not a frame. A sender that
// fragments past the limit must still be refused, or the limit is decorative.
func TestReadLimitSpansFragments(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server, connectwebsocket.WithReadMaxBytes(1024))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	addr := httpServer.Listener.Addr().String()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_ = rawHandshake(t, conn, addr, pingv1connect.PingServiceCumSumProcedure)
	assert.Nil(t, conn.SetDeadline(time.Now().Add(20*time.Second)))
	assert.Nil(t, writeClientFragment(conn, []byte("M{}"), true, opcodeText))

	// 4 KiB in 64-byte pieces: every frame is far under the limit, and the
	// message they reassemble into is four times over it.
	const fragmentBytes = 64
	chunk := make([]byte, fragmentBytes)
	assert.Nil(t, writeClientFragment(conn, append([]byte("B"), chunk...), false, opcodeBinary))
	for range 64 {
		if err := writeClientFragment(conn, chunk, false, opcodeContinuation); err != nil {
			break // the server stopped reading, which is the point
		}
	}

	_, payload := readServerFrame(t, conn)
	assert.Equal(t, rune(payload[0]), wireServerEndStream)
	assert.True(t, strings.Contains(string(payload), "resource_exhausted"))
	assert.True(t, strings.Contains(string(payload), "configured max of 1024 bytes"))
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
	// The number a caller configured, not the WebSocket library's wire limit,
	// which is four bytes larger to leave room for the marker.
	assert.True(t, strings.Contains(err.Error(), "configured max of 1024 bytes"))
}

// A client enforces its limit per reassembled message too, and classifies the
// overrun the same way a server does so one monitor sees both directions.
func TestClientReportsSizeLimitFaults(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, echoSizeServer{responseBytes: 64 * 1024})
	recorder := &clientFaultRecorder{}
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
		connectwebsocket.WithReadMaxBytes(1024),
		connectwebsocket.WithClientProtocolErrorHandler(recorder.handle),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
	assert.NotNil(t, err)

	faults, _ := recorder.seen()
	assert.Equal(t, len(faults), 1)
	assert.Equal(t, faults[0], connectwebsocket.FaultSizeLimit)
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
