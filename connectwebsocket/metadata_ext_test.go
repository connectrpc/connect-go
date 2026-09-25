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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
)

// newServerFor mounts an arbitrary handler on both transports, which
// newHybridServer cannot do because it takes the concrete pingServer.
func newServerFor(
	tb testing.TB,
	handler pingv1connect.PingServiceHandler,
	options ...connectwebsocket.ServerOption,
) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, handler)
	mux := http.NewServeMux()
	connecthttp.Mount(
		connectwebsocket.Mux(mux, server, options...),
		server,
	)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// trailerShapeServer sets leading and trailing metadata of every shape a caller
// can produce: repeated values, and a header alongside a trailer.
type trailerShapeServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (trailerShapeServer) CountUp(
	ctx context.Context,
	_ *pingv1.CountUpRequest,
	stream pingv1connect.PingServiceCountUpServerStream,
) error {
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		info.ResponseHeader().Set("Lead", "header-value")
		info.ResponseTrailer().Add("Multi", "one")
		info.ResponseTrailer().Add("Multi", "two")
	}
	return stream.Send(&pingv1.CountUpResponse{Number: 1})
}

// drainCountUp runs the RPC to completion, which is what makes metadata
// available.
func drainCountUp(ctx context.Context, tb testing.TB, client pingv1connect.PingServiceClient) {
	tb.Helper()
	stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 1})
	assert.Nil(tb, err)
	for {
		if _, err := stream.Receive(); err != nil {
			break
		}
	}
	assert.Nil(tb, stream.Close())
}

// A trailer set twice must arrive twice, in order. The end-stream metadata is
// JSON, where a repeated key is an array rather than a second entry.
func TestRepeatedTrailerValuesSurvive(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, trailerShapeServer{})
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	ctx, info := connect.NewClientContext(t.Context())
	drainCountUp(ctx, t, client)
	assert.Equal(t, info.ResponseTrailer().Values("Multi"), []string{"one", "two"})
}

// Leading metadata a handler sets must reach the caller as a *header*, the
// same as it does over HTTP. The 101 is written before the handler runs, so
// there is no response header block to carry it; it travels in a
// Leading-Metadata message instead. Asserted against connecthttp rather than
// against a constant, so the two transports cannot drift apart.
func TestResponseHeadersArriveAsHeaders(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, trailerShapeServer{})

	wsTransport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	assert.Nil(t, err)
	wsCtx, wsInfo := connect.NewClientContext(t.Context())
	drainCountUp(wsCtx, t, pingv1connect.NewPingServiceClient(connect.NewClient(wsTransport)))

	httpCtx, httpInfo := connect.NewClientContext(t.Context())
	drainCountUp(httpCtx, t, pingv1connect.NewPingServiceClient(connect.NewClient(
		connecthttp.NewTransport(httpServer.Client(), httpServer.URL),
	)))

	assert.Equal(t, wsInfo.ResponseHeader().Values("Lead"), []string{"header-value"})
	assert.Equal(t, wsInfo.ResponseHeader().Values("Lead"), httpInfo.ResponseHeader().Values("Lead"))
	// And it is not also delivered as a trailer: one bag on the wire, but the
	// two halves stay distinct at the API.
	assert.Equal(t, len(wsInfo.ResponseTrailer().Values("Lead")), 0)
	assert.Equal(t, len(httpInfo.ResponseTrailer().Values("Lead")), 0)
}

// Headers must be readable at the same point in the stream as they are over
// HTTP: not before the first Receive, and yes after it.
func TestLeadingMetadataIsReadableAfterFirstReceive(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, trailerShapeServer{})

	readAfterFirstReceive := func(transport connect.Transport) (before, after []string) {
		ctx, info := connect.NewClientContext(t.Context())
		client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
		stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 1})
		assert.Nil(t, err)
		t.Cleanup(func() { _ = stream.Close() })
		before = info.ResponseHeader().Values("Lead")
		_, err = stream.Receive()
		assert.Nil(t, err)
		return before, info.ResponseHeader().Values("Lead")
	}

	wsTransport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	assert.Nil(t, err)
	wsBefore, wsAfter := readAfterFirstReceive(wsTransport)
	httpBefore, httpAfter := readAfterFirstReceive(
		connecthttp.NewTransport(httpServer.Client(), httpServer.URL))

	assert.Equal(t, len(wsBefore), len(httpBefore))
	assert.Equal(t, wsAfter, httpAfter)
	assert.Equal(t, wsAfter, []string{"header-value"})
}

// failingMetadataServer sets leading metadata and then fails without sending a
// message: the case where the metadata has nothing to ride ahead of, and would
// be lost if the metadata only flushed on the first Send.
type failingMetadataServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (failingMetadataServer) CountUp(
	ctx context.Context,
	_ *pingv1.CountUpRequest,
	_ pingv1connect.PingServiceCountUpServerStream,
) error {
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		info.ResponseHeader().Set("Lead", "header-value")
		info.ResponseTrailer().Set("Trail", "trailer-value")
	}
	return connect.NewError(connect.CodeUnavailable, "no messages for you")
}

// The flush point is "before the first body, or before the end-of-stream
// message if there is no body" — this is the second clause.
func TestLeadingMetadataSurvivesAStreamWithNoMessages(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, failingMetadataServer{})
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	ctx, info := connect.NewClientContext(t.Context())
	stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 1})
	assert.Nil(t, err)
	t.Cleanup(func() { _ = stream.Close() })
	_, err = stream.Receive()
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable)

	assert.Equal(t, info.ResponseHeader().Values("Lead"), []string{"header-value"})
	assert.Equal(t, info.ResponseTrailer().Values("Trail"), []string{"trailer-value"})
}

// A stream carries exactly one M message and it opens the stream, so a later
// one is refused. It has to be: the opening message is
// consumed before the handler is dispatched, and a second would write the
// handler's own request metadata while it is running.
func TestLateLeadingMetadataIsRejected(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	conn := dialCumSum(t, httpServer, "")

	// A body first, then metadata: the illegal order.
	sendProtoBody(t, conn, &pingv1.CumSumRequest{Number: 1})
	reply := readServerOpening(t, conn) // past the server's own M, to the handler's reply
	assert.Equal(t, rune(reply[0]), wireBody)

	sendJSONMessage(t, conn, wireMetadata, []byte(`{"Acme-Late":["nope"]}`))
	_, data, err := conn.Read(t.Context())
	assert.Nil(t, err)
	assert.True(t, strings.Contains(string(data), "invalid_argument"))
	assert.True(t, strings.Contains(string(data), "second M message"))
}

// Every response stream opens with exactly one M, even when the handler sets
// no metadata at all. The empty message is not waste: it is what tells a
// client the server has accepted the stream and begun, which the response
// header block does for free over HTTP.
func TestServerAlwaysOpensWithLeadingMetadata(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	conn := dialCumSum(t, httpServer, "")
	sendProtoBody(t, conn, &pingv1.CumSumRequest{Number: 1})

	messageType, opening, err := conn.Read(t.Context())
	assert.Nil(t, err)
	assert.Equal(t, messageType, websocket.MessageText) // JSON, so a text frame
	assert.Equal(t, string(opening), "M{}")             // empty, but present
}

// A server that sent a second M would be rewriting headers the application may
// already have read — the race the request side refuses for the same reason.
func TestSecondServerLeadingMetadataIsRejected(t *testing.T) {
	t.Parallel()
	httpServer := misframingServer(t, func(conn *websocket.Conn) {
		// misframingServer already sent the opening M; this is the second.
		_ = conn.Write(context.Background(), websocket.MessageText, []byte(`M{"acme-late":["nope"]}`))
	})
	recorder := &clientFaultRecorder{}
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithClientProtocolErrorHandler(recorder.handle),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	t.Cleanup(func() { _ = stream.Close() })
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))

	_, receiveErr := stream.Receive()
	assert.NotNil(t, receiveErr)
	assert.True(t, strings.Contains(receiveErr.Error(), "second M message"))

	faults, _ := recorder.seen()
	assert.Equal(t, len(faults), 1)
	assert.Equal(t, faults[0], connectwebsocket.FaultMetadata)
}
