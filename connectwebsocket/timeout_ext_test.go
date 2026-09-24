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
	"net/url"
	"strconv"
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

// dialCumSum opens a raw CumSum stream with the given query string, so a test
// can drive the wire directly rather than through the Go client.
func dialCumSum(tb testing.TB, httpServer *httptest.Server, query string) *websocket.Conn {
	tb.Helper()
	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") +
		pingv1connect.PingServiceCumSumProcedure + query
	conn, res, err := websocket.Dial(tb.Context(), url, &websocket.DialOptions{
		HTTPClient:   httpServer.Client(),
		Subprotocols: []string{"connectrpc.1+proto"},
	})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	assert.Nil(tb, err)
	tb.Cleanup(func() { _ = conn.CloseNow() })
	// Every stream opens with Leading-Metadata; see the protocol's ordering rule.
	sendJSONMessage(tb, conn, wireMetadata, []byte("{}"))
	return conn
}

// sendProtoBody writes one Protobuf body, which starts the RPC. Protobuf means
// a binary frame.
func sendProtoBody(tb testing.TB, conn *websocket.Conn, msg proto.Message) {
	tb.Helper()
	payload, err := proto.Marshal(msg)
	assert.Nil(tb, err)
	sendWireMessage(tb, conn, false, wireBody, payload)
}

// sendJSONMessage writes one control message: JSON, hence a text frame.
func sendJSONMessage(tb testing.TB, conn *websocket.Conn, marker rune, payload []byte) {
	tb.Helper()
	sendWireMessage(tb, conn, true, marker, payload)
}

// sendWireMessage writes one marker and payload, so a test can produce a
// message the Go client would never send.
func sendWireMessage(tb testing.TB, conn *websocket.Conn, text bool, marker rune, payload []byte) {
	tb.Helper()
	frame := append([]byte(string(marker)), payload...)
	messageType := websocket.MessageBinary
	if text {
		messageType = websocket.MessageText
	}
	assert.Nil(tb, conn.Write(tb.Context(), messageType, frame))
}

// The query parameter has to produce a real deadline on the handler's context,
// not merely be accepted.
func TestTimeoutQueryParameterReachesHandler(t *testing.T) {
	t.Parallel()
	deadlines := make(chan time.Duration, 1)
	httpServer := newHybridServer(t, pingServer{sawDeadline: deadlines})
	conn := dialCumSum(t, httpServer, "?connect-timeout-ms=1500")
	sendProtoBody(t, conn, &pingv1.CumSumRequest{Number: 1})

	select {
	case remaining := <-deadlines:
		assert.True(t, remaining > 0)
		assert.True(t, remaining <= 1500*time.Millisecond)
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}
}

// A client that requests no deadline still gets one: the server's default
// bounds every RPC, so a peer cannot hold a connection open forever.
func TestNoClientTimeoutFallsBackToTheServerDefault(t *testing.T) {
	t.Parallel()
	deadlines := make(chan time.Duration, 1)
	httpServer := newHybridServer(t, pingServer{sawDeadline: deadlines})
	conn := dialCumSum(t, httpServer, "")
	sendProtoBody(t, conn, &pingv1.CumSumRequest{Number: 1})

	select {
	case remaining := <-deadlines:
		// The package default, minus the flight time to get here.
		assert.True(t, remaining > 0)
		assert.True(t, remaining <= time.Hour)
		assert.True(t, remaining > 59*time.Minute)
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}
}

// The query parameter is the deadline's only channel. A Connect-Timeout-Ms
// header is ordinary request metadata here, so a header that disagrees with
// the query parameter does not shorten the RPC — and does not fail it either.
func TestTimeoutHeaderIsNotADeadline(t *testing.T) {
	t.Parallel()
	deadlines := make(chan time.Duration, 1)
	httpServer := newHybridServer(t, pingServer{sawDeadline: deadlines})
	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") +
		pingv1connect.PingServiceCumSumProcedure + "?connect-timeout-ms=1500"
	conn, res, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{
		HTTPClient:   httpServer.Client(),
		Subprotocols: []string{"connectrpc.1+proto"},
		HTTPHeader:   map[string][]string{"Connect-Timeout-Ms": {"9000"}},
	})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	assert.Nil(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })
	sendJSONMessage(t, conn, wireMetadata, []byte("{}"))
	sendProtoBody(t, conn, &pingv1.CumSumRequest{Number: 1})

	select {
	case remaining := <-deadlines:
		// The query parameter's 1.5s, not the header's 9s.
		assert.True(t, remaining <= 1500*time.Millisecond)
		assert.True(t, remaining > time.Second)
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}
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
		Subprotocols: []string{"connectrpc.1+proto"},
	})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	assert.Nil(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })
	sendJSONMessage(t, conn, wireMetadata, []byte("{}"))
	sendProtoBody(t, conn, &pingv1.CountUpRequest{Number: 1})

	_, data, err := conn.Read(t.Context())
	assert.Nil(t, err)
	// An S message carrying the verdict, not a mute close.
	assert.True(t, len(data) > 5)
	assert.Equal(t, rune(data[0]), wireServerEndStream)
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

// The Go client must put its deadline where the protocol says it goes. Nothing
// caught the header-only client for a long while, because every test that
// checked the deadline drove the wire by hand and every test that used the Go
// client had this server on the other end, which accepted both.
func TestGoClientSendsTheTimeoutQueryParameter(t *testing.T) {
	t.Parallel()
	queries := make(chan string, 1)
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case queries <- request.URL.RawQuery:
		default:
		}
	}))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// The handshake fails — this server never upgrades — but the request it
	// refused is the one under test.
	_, _ = client.Ping(ctx, &pingv1.PingRequest{Number: 1})

	select {
	case query := <-queries:
		values, parseErr := url.ParseQuery(query)
		assert.Nil(t, parseErr)
		millis, convErr := strconv.Atoi(values.Get("connect-timeout-ms"))
		assert.Nil(t, convErr)
		assert.True(t, millis > 29_000)
		assert.True(t, millis <= 30_000)
	case <-time.After(5 * time.Second):
		t.Fatal("the client never sent a handshake")
	}
}

// A context with no deadline sends no parameter at all, rather than a zero
// that would mean an instantly expired RPC.
func TestGoClientOmitsTheParameterWithoutADeadline(t *testing.T) {
	t.Parallel()
	queries := make(chan string, 1)
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case queries <- request.URL.RawQuery:
		default:
		}
	}))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	_, _ = client.Ping(context.Background(), &pingv1.PingRequest{Number: 1})

	select {
	case query := <-queries:
		assert.Equal(t, query, "")
	case <-time.After(5 * time.Second):
		t.Fatal("the client never sent a handshake")
	}
}
