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
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
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

// Envelope flag bits, restated here so the test asserts against the wire
// format rather than against the implementation's constants.
const (
	flagEndStream       = 0b00000010 // server -> client
	flagEndClientStream = 0b00000100 // client -> server
	flagLeadingMetadata = 0b00001000 // client -> server
)

// browserClient is the hand-rolled stand-in for a browser: it speaks the wire
// format directly, including the two client-only flag bits the Go client never
// sends. Without it the browser paths have no coverage at all.
type browserClient struct {
	conn *websocket.Conn
}

func dialBrowserClient(tb testing.TB, server *httptest.Server, procedure string) *browserClient {
	tb.Helper()
	return dialBrowserClientWithToken(tb, server, procedure, "connect.v2")
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
	return &browserClient{conn: conn}
}

// writeEnvelope frames one envelope into one binary message.
func (c *browserClient) writeEnvelope(tb testing.TB, flags uint8, payload []byte) {
	tb.Helper()
	frame := make([]byte, 5+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
		tb.Fatalf("write envelope: %v", err)
	}
}

// readEnvelope reads one binary message and splits off the 5-byte prefix.
func (c *browserClient) readEnvelope(tb testing.TB) (uint8, []byte) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	messageType, data, err := c.conn.Read(ctx)
	if err != nil {
		tb.Fatalf("read envelope: %v", err)
	}
	assert.Equal(tb, messageType, websocket.MessageBinary)
	if len(data) < 5 {
		tb.Fatalf("short envelope: %d bytes", len(data))
	}
	size := binary.BigEndian.Uint32(data[1:5])
	assert.Equal(tb, int(size), len(data)-5)
	return data[0], data[5:]
}

// metadataServer records the request metadata the handler observed.
type metadataServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	seen chan http.Header
}

func (m metadataServer) CumSum(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	// Receive first: Leading-Metadata envelopes arrive before the first data
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

// TestBrowserLeadingMetadataEnvelope drives the bit-3 path: a client that
// cannot set headers on the upgrade sends them as an envelope instead.
func TestBrowserLeadingMetadataEnvelope(t *testing.T) {
	t.Parallel()
	seen := make(chan http.Header, 1)
	client := dialBrowserClient(t, newMetadataServer(t, seen), pingProcedure)

	metadata, err := json.Marshal(map[string][]string{
		"Acme-Tenant": {"tenant-42"},
	})
	assert.Nil(t, err)
	client.writeEnvelope(t, flagLeadingMetadata, metadata)

	request, err := proto.Marshal(&pingv1.CumSumRequest{Number: 3})
	assert.Nil(t, err)
	client.writeEnvelope(t, 0, request)

	flags, payload := client.readEnvelope(t)
	assert.Equal(t, flags, uint8(0))
	var response pingv1.CumSumResponse
	assert.Nil(t, proto.Unmarshal(payload, &response))
	assert.Equal(t, response.Sum, int64(3))

	header := <-seen
	assert.Equal(t, header.Get("Acme-Tenant"), "tenant-42")
}

// TestBrowserEndOfClientStreamEnvelope drives the bit-2 path, which stands in
// for the request-body EOF a WebSocket cannot send.
func TestBrowserEndOfClientStreamEnvelope(t *testing.T) {
	t.Parallel()
	seen := make(chan http.Header, 1)
	client := dialBrowserClient(t, newMetadataServer(t, seen), pingProcedure)

	request, err := proto.Marshal(&pingv1.CumSumRequest{Number: 5})
	assert.Nil(t, err)
	client.writeEnvelope(t, 0, request)
	flags, payload := client.readEnvelope(t)
	assert.Equal(t, flags, uint8(0))
	var response pingv1.CumSumResponse
	assert.Nil(t, proto.Unmarshal(payload, &response))
	assert.Equal(t, response.Sum, int64(5))
	<-seen

	// Empty bit-2 envelope: the handler should see io.EOF and return, which
	// makes the server send its EndStream envelope.
	client.writeEnvelope(t, flagEndClientStream, nil)
	endFlags, endPayload := client.readEnvelope(t)
	assert.Equal(t, endFlags, uint8(flagEndStream))
	var end map[string]any
	assert.Nil(t, json.Unmarshal(endPayload, &end))
	// A clean finish carries no error member.
	_, hasError := end["error"]
	assert.False(t, hasError)
}

// TestBrowserRejectsServerOnlyFlag pins the direction check: a client must not
// be able to send the server's EndStream flag.
func TestBrowserRejectsServerOnlyFlag(t *testing.T) {
	t.Parallel()
	seen := make(chan http.Header, 1)
	client := dialBrowserClient(t, newMetadataServer(t, seen), pingProcedure)

	client.writeEnvelope(t, flagEndStream, []byte("{}"))
	// The server rejects it and terminates the RPC with an EndStream envelope
	// carrying the error.
	flags, payload := client.readEnvelope(t)
	assert.Equal(t, flags, uint8(flagEndStream))
	var end map[string]any
	assert.Nil(t, json.Unmarshal(payload, &end))
	_, hasError := end["error"]
	assert.True(t, hasError)
}
