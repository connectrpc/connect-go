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
	"encoding/binary"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

// dialCumSum opens a raw CumSum stream with the given query string. A browser
// cannot set headers on a WebSocket handshake, so the query is the only place
// it can put a deadline — and the Go client never exercises that path.
func dialCumSum(tb testing.TB, httpServer *httptest.Server, query string) *websocket.Conn {
	tb.Helper()
	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") +
		pingv1connect.PingServiceCumSumProcedure + query
	conn, res, err := websocket.Dial(tb.Context(), url, &websocket.DialOptions{
		HTTPClient:   httpServer.Client(),
		Subprotocols: []string{"connect.v2+proto"},
	})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	assert.Nil(tb, err)
	tb.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// sendEnvelope writes one flagless data envelope, which is what starts the RPC.
func sendEnvelope(tb testing.TB, conn *websocket.Conn, msg proto.Message) {
	tb.Helper()
	payload, err := proto.Marshal(msg)
	assert.Nil(tb, err)
	sendRawEnvelope(tb, conn, 0, payload)
}

// sendRawEnvelope writes one envelope with the given flags byte, so a test can
// produce a frame the Go client would never send.
func sendRawEnvelope(tb testing.TB, conn *websocket.Conn, flags byte, payload []byte) {
	tb.Helper()
	frame := make([]byte, 5+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	assert.Nil(tb, conn.Write(tb.Context(), websocket.MessageBinary, frame))
}

// The query parameter has to produce a real deadline on the handler's context,
// not merely be accepted.
func TestTimeoutQueryParameterReachesHandler(t *testing.T) {
	t.Parallel()
	deadlines := make(chan time.Duration, 1)
	httpServer := newHybridServer(t, pingServer{sawDeadline: deadlines})
	conn := dialCumSum(t, httpServer, "?connect-timeout-ms=1500")
	sendEnvelope(t, conn, &pingv1.CumSumRequest{Number: 1})

	select {
	case remaining := <-deadlines:
		assert.True(t, remaining > 0)
		assert.True(t, remaining <= 1500*time.Millisecond)
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}
}

// With no deadline anywhere, the handler must see none rather than a zero one.
func TestNoTimeoutLeavesHandlerWithoutDeadline(t *testing.T) {
	t.Parallel()
	deadlines := make(chan time.Duration, 1)
	httpServer := newHybridServer(t, pingServer{sawDeadline: deadlines})
	conn := dialCumSum(t, httpServer, "")
	sendEnvelope(t, conn, &pingv1.CumSumRequest{Number: 1})

	select {
	case remaining := <-deadlines:
		assert.Equal(t, remaining, time.Duration(0))
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}
}

// A header and a query parameter that disagree are ambiguous, and guessing
// would silently shorten or extend someone's deadline.
func TestConflictingTimeoutsAreRejected(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") +
		pingv1connect.PingServiceCumSumProcedure + "?connect-timeout-ms=1500"
	conn, res, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{
		HTTPClient:   httpServer.Client(),
		Subprotocols: []string{"connect.v2+proto"},
		HTTPHeader:   map[string][]string{"Connect-Timeout-Ms": {"9000"}},
	})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	assert.Nil(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })

	// The handshake succeeds — it is already committed — so the complaint
	// arrives as an EndStream envelope.
	_, data, err := conn.Read(t.Context())
	assert.Nil(t, err)
	assert.True(t, strings.Contains(string(data), "invalid_argument"))
	assert.True(t, strings.Contains(string(data), "conflicting"))
}

// slowDeadlineServer overruns its deadline while computing rather than while
// blocked in a read, so the socket is still alive when the context dies. It
// never calls Receive, so no read is pending to be torn down.
type slowDeadlineServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (slowDeadlineServer) CountUp(
	ctx context.Context,
	_ *pingv1.CountUpRequest,
	_ pingv1connect.PingServiceCountUpServerStream,
) error {
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond)
	return ctx.Err()
}

// A server whose deadline expires must still be able to say so. The terminal
// write runs detached from the RPC's context for exactly this reason: writing
// under the expired one fails, and the peer is left with a dead connection and
// no verdict, which it can only report as a transport failure.
func TestServerReportsItsOwnExpiredDeadline(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, slowDeadlineServer{})
	// A raw client, so that only the server's deadline is in play: the Go
	// client derives the server's deadline from its own, and always hears its
	// own fire first.
	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") +
		pingv1connect.PingServiceCountUpProcedure + "?connect-timeout-ms=200"
	conn, res, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{
		HTTPClient:   httpServer.Client(),
		Subprotocols: []string{"connect.v2+proto"},
	})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	assert.Nil(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })
	sendEnvelope(t, conn, &pingv1.CountUpRequest{Number: 1})

	_, data, err := conn.Read(t.Context())
	assert.Nil(t, err)
	// An EndStream envelope carrying the verdict, not a mute close.
	assert.True(t, len(data) > 5)
	assert.Equal(t, data[0], byte(0b00000010))
	assert.True(t, strings.Contains(string(data), "deadline exceeded"))
}

// newBidiClientWithTimeouts builds a plain WebSocket-backed client for the
// deadline tests, which care about time rather than about routing.
func newBidiClientWithTimeouts(tb testing.TB) pingv1connect.PingServiceClient {
	tb.Helper()
	httpServer := newHybridServer(tb, pingServer{})
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	assert.Nil(tb, err)
	return pingv1connect.NewPingServiceClient(connect.NewClient(transport))
}

// A stream that is slow but never idle for longer than its deadline must run
// to completion. The deadline bounds the whole RPC, not the gap between
// messages, and nothing in the transport may shorten it.
func TestSlowStreamSurvivesWithinItsDeadline(t *testing.T) {
	t.Parallel()
	client := newBidiClientWithTimeouts(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	var want int64
	for i := int64(1); i <= 5; i++ {
		time.Sleep(120 * time.Millisecond)
		assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: i}))
		want += i
		response, err := stream.Receive()
		assert.Nil(t, err)
		assert.Equal(t, response.Sum, want)
	}

	assert.Nil(t, stream.CloseSend())
	_, err = stream.Receive()
	assert.True(t, errors.Is(err, io.EOF))
	assert.Nil(t, stream.Close())
}

// With no deadline anywhere, an idle stream must stay open. There is no
// keep-alive and no idle timeout in this transport; a stream that died here
// would mean something had grown one.
func TestIdleStreamWithoutDeadlineStaysAlive(t *testing.T) {
	t.Parallel()
	client := newBidiClientWithTimeouts(t)

	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 4}))
	response, err := stream.Receive()
	assert.Nil(t, err)
	assert.Equal(t, response.Sum, int64(4))

	// Idle long enough that an accidental short timeout would trip. The server
	// spends this blocked in Receive.
	time.Sleep(1500 * time.Millisecond)

	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 6}))
	response, err = stream.Receive()
	assert.Nil(t, err)
	assert.Equal(t, response.Sum, int64(10))

	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())
}

// The other half of the same property: a stream that goes idle with a deadline
// must fail when the deadline arrives, and not before. Only the lower bound is
// asserted; a loaded machine can be late, but it must never be early.
func TestIdleStreamFailsAtItsDeadlineAndNotBefore(t *testing.T) {
	t.Parallel()
	client := newBidiClientWithTimeouts(t)
	const timeout = 900 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	// Make progress first, so the failure cannot be blamed on a stream that
	// never worked.
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 2}))
	response, err := stream.Receive()
	assert.Nil(t, err)
	assert.Equal(t, response.Sum, int64(2))

	start := time.Now()
	_, err = stream.Receive()
	elapsed := time.Since(start)

	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded)
	// Generous: the send and first receive consumed some of the budget, so the
	// blocked read cannot have waited the full timeout.
	assert.True(t, elapsed > timeout/2)
	_ = stream.Close()
}
