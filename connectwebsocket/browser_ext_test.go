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
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

// Message markers, restated here so the tests assert against the wire format
// rather than against the implementation's constants.
const (
	wireBody            = 'B' // either direction
	wireMetadata        = 'M' // either direction
	wireServerEndStream = 'S' // server -> client
	wireClientEndStream = 'C' // client -> server
)

// browserClient is the hand-rolled stand-in for a browser: it speaks the wire
// format directly, including the two client-only flag bits the Go client never
// sends. Without it the browser paths have no coverage at all.
type browserClient struct {
	conn *websocket.Conn
}

func dialBrowserClient(tb testing.TB, server *httptest.Server, procedure string) *browserClient {
	tb.Helper()
	return dialBrowserClientWithToken(tb, server, procedure, "connectrpc.1+proto")
}

// dialBrowserClientWithToken offers one specific subprotocol, for tests that
// care which codec the token selects rather than taking the default.
func dialBrowserClientWithToken(
	tb testing.TB,
	server *httptest.Server,
	procedure string,
	token string,
) *browserClient {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	tb.Cleanup(cancel)
	conn, res, err := websocket.Dial(ctx, "ws"+server.URL[4:]+procedure, &websocket.DialOptions{
		HTTPClient:   server.Client(),
		Subprotocols: []string{token},
	})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		tb.Fatalf("dial: %v", err)
	}
	tb.Cleanup(func() { _ = conn.CloseNow() })
	client := &browserClient{conn: conn}
	// Every stream opens with exactly one M message, empty when
	// there is none. Tests with metadata of their own use dialBrowserClientOpening.
	client.writeJSON(tb, wireMetadata, []byte("{}"))
	return client
}

// dialBrowserClientOpening opens the stream with the given metadata payload
// rather than an empty one — a stream carries exactly one such message, so a
// test cannot send its metadata as a second.
func dialBrowserClientOpening(
	tb testing.TB,
	server *httptest.Server,
	procedure string,
	metadata []byte,
) *browserClient {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	tb.Cleanup(cancel)
	conn, res, err := websocket.Dial(ctx, "ws"+server.URL[4:]+procedure, &websocket.DialOptions{
		HTTPClient:   server.Client(),
		Subprotocols: []string{"connectrpc.1+proto"},
	})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		tb.Fatalf("dial: %v", err)
	}
	tb.Cleanup(func() { _ = conn.CloseNow() })
	client := &browserClient{conn: conn}
	client.writeJSON(tb, wireMetadata, metadata)
	return client
}

// writeMessage frames one marker and payload into one WebSocket message. text
// picks the frame type, which is what tells the peer how to read the payload.
func (c *browserClient) writeMessage(tb testing.TB, marker rune, text bool, payload []byte) {
	tb.Helper()
	frame := append([]byte(string(marker)), payload...)
	messageType := websocket.MessageBinary
	if text {
		messageType = websocket.MessageText
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, messageType, frame); err != nil {
		tb.Fatalf("write message: %v", err)
	}
}

// writeJSON sends a control message, which is always JSON in a text frame.
func (c *browserClient) writeJSON(tb testing.TB, marker rune, payload []byte) {
	tb.Helper()
	c.writeMessage(tb, marker, true, payload)
}

// writeProto sends a body encoded as Protobuf, hence a binary frame.
func (c *browserClient) writeProto(tb testing.TB, payload []byte) {
	tb.Helper()
	c.writeMessage(tb, wireBody, false, payload)
}

// readMessage reads one message and splits off its marker, reporting whether
// it arrived in a text frame.
func (c *browserClient) readMessage(tb testing.TB) (rune, []byte, bool) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	messageType, data, err := c.conn.Read(ctx)
	if err != nil {
		tb.Fatalf("read message: %v", err)
	}
	if len(data) == 0 {
		tb.Fatal("empty message carries no marker")
	}
	marker, size := utf8.DecodeRune(data)
	return marker, data[size:], messageType == websocket.MessageText
}

// metadataServer records the request metadata the handler observed.
type metadataServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	seen chan http.Header
}

func (m metadataServer) CumSum(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	// Receive first: M messages arrive before the first body
	// message, so the metadata is only complete after a read.
	req, err := stream.Receive()
	if err != nil {
		return err
	}
	header := http.Header{}
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		maps.Insert(header, info.RequestHeader().All())
	}
	m.seen <- header
	return stream.Send(&pingv1.CumSumResponse{Sum: req.Number})
}

func newMetadataServer(tb testing.TB, seen chan http.Header) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, metadataServer{seen: seen})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// TestBrowserLeadingMetadataMessage drives the M path: a client that cannot
// set headers on the upgrade sends them as a message instead.
func TestBrowserLeadingMetadataMessage(t *testing.T) {
	t.Parallel()
	seen := make(chan http.Header, 1)
	metadata, err := json.Marshal(map[string][]string{
		"Acme-Tenant": {"tenant-42"},
	})
	assert.Nil(t, err)
	client := dialBrowserClientOpening(t, newMetadataServer(t, seen), pingProcedure, metadata)

	request, err := proto.Marshal(&pingv1.CumSumRequest{Number: 3})
	assert.Nil(t, err)
	client.writeProto(t, request)

	// Every response stream opens with the server's own M, even empty.
	opening, _, openingText := client.readMessage(t)
	assert.Equal(t, opening, wireMetadata)
	assert.True(t, openingText) // metadata is JSON, so a text frame

	marker, payload, text := client.readMessage(t)
	assert.Equal(t, marker, wireBody)
	assert.False(t, text) // a Protobuf body is a binary frame
	var response pingv1.CumSumResponse
	assert.Nil(t, proto.Unmarshal(payload, &response))
	assert.Equal(t, response.Sum, int64(3))

	header := <-seen
	assert.Equal(t, header.Get("Acme-Tenant"), "tenant-42")
}

// TestBrowserEndOfClientStreamMessage drives the C path, which stands in for
// the request-body EOF a WebSocket cannot send.
func TestBrowserEndOfClientStreamMessage(t *testing.T) {
	t.Parallel()
	seen := make(chan http.Header, 1)
	client := dialBrowserClient(t, newMetadataServer(t, seen), pingProcedure)

	request, err := proto.Marshal(&pingv1.CumSumRequest{Number: 5})
	assert.Nil(t, err)
	client.writeProto(t, request)
	opening, _, _ := client.readMessage(t)
	assert.Equal(t, opening, wireMetadata)
	marker, payload, _ := client.readMessage(t)
	assert.Equal(t, marker, wireBody)
	var response pingv1.CumSumResponse
	assert.Nil(t, proto.Unmarshal(payload, &response))
	assert.Equal(t, response.Sum, int64(5))
	<-seen

	// A bare C: the handler should see io.EOF and return, which makes the
	// server send its S message.
	client.writeJSON(t, wireClientEndStream, nil)
	endMarker, endPayload, endText := client.readMessage(t)
	assert.Equal(t, endMarker, wireServerEndStream)
	assert.True(t, endText) // EndStreamMessage is JSON, so a text frame
	var end map[string]any
	assert.Nil(t, json.Unmarshal(endPayload, &end))
	// A clean finish carries no error member.
	_, hasError := end["error"]
	assert.False(t, hasError)
}

// TestBrowserRejectsServerOnlyMarker pins the direction check: a client must
// not be able to send the server's end-of-stream marker.
func TestBrowserRejectsServerOnlyMarker(t *testing.T) {
	t.Parallel()
	seen := make(chan http.Header, 1)
	client := dialBrowserClient(t, newMetadataServer(t, seen), pingProcedure)

	client.writeJSON(t, wireServerEndStream, []byte("{}"))
	// The server rejects it and terminates the RPC with an S message carrying
	// the error, behind its own opening M.
	opening, _, _ := client.readMessage(t)
	assert.Equal(t, opening, wireMetadata)
	marker, payload, _ := client.readMessage(t)
	assert.Equal(t, marker, wireServerEndStream)
	var end map[string]any
	assert.Nil(t, json.Unmarshal(payload, &end))
	_, hasError := end["error"]
	assert.True(t, hasError)
}

// writeProtoEnd sends the final client message carrying a Protobuf body, which
// follows the codec and is therefore a binary frame.
func (c *browserClient) writeProtoEnd(tb testing.TB, payload []byte) {
	tb.Helper()
	c.writeMessage(tb, wireClientEndStream, false, payload)
}
