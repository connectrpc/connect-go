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
	"errors"
	"io"
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
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

// observationServer reports the same header twice: once before it has received
// anything, once after. The pair is the whole point — a difference between them
// is a write to CallInfo that happened after the handler was already running.
type observationServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	observations chan string
}

func (s observationServer) CumSum(
	ctx context.Context,
	stream pingv1connect.PingServiceCumSumServerStream,
) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	s.observations <- info.RequestHeader().Get("Acme-Tenant")
	for {
		request, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		s.observations <- info.RequestHeader().Get("Acme-Tenant")
		if err := stream.Send(&pingv1.CumSumResponse{Sum: request.Number}); err != nil {
			return err
		}
	}
}

func newObservationServer(tb testing.TB, observations chan string) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, observationServer{observations: observations})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// Leading-Metadata is drained before the handler is dispatched, so CallInfo is
// complete on the handler's very first line and never changes afterwards.
//
// That ordering is what interceptors depend on — an auth interceptor runs
// before the handler and must see the client's credentials — and it is what
// makes the header map safe to read from a second goroutine, since nothing
// writes it once the handler exists.
func TestLeadingMetadataIsCompleteBeforeTheHandlerStarts(t *testing.T) {
	t.Parallel()
	observations := make(chan string, 2)
	httpServer := newObservationServer(t, observations)

	metadata, err := json.Marshal(map[string][]string{"Acme-Tenant": {"tenant-42"}})
	assert.Nil(t, err)
	client := dialBrowserClientOpening(
		t, httpServer, pingv1connect.PingServiceCumSumProcedure, metadata)
	payload, err := proto.Marshal(&pingv1.CumSumRequest{Number: 1})
	assert.Nil(t, err)
	client.writeProto(t, payload)
	client.readMessage(t)

	timeout := time.After(5 * time.Second)
	read := func(what string) string {
		t.Helper()
		select {
		case observed := <-observations:
			return observed
		case <-timeout:
			t.Fatalf("handler never reported %s", what)
			return ""
		}
	}

	// Present before the handler receives anything: the session drained the
	// message while building the call.
	assert.Equal(t, read("its first observation"), "tenant-42")
	// And unchanged across a Receive, because nothing writes it any more.
	assert.Equal(t, read("its second observation"), "tenant-42")
}

// racingServer is the ordinary bidi shape: one goroutine receives while another
// reads request metadata. Before the opening message was drained ahead of
// dispatch, that pair raced — Receive merged metadata into the same CallInfo
// map the other goroutine was reading.
type racingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (racingServer) CumSum(
	ctx context.Context,
	stream pingv1connect.PingServiceCumSumServerStream,
) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100_000 {
			_ = info.RequestHeader().Get("Acme-Tenant")
		}
	}()
	defer func() { <-done }()
	for {
		request, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		// Echo, so the test has a synchronization point and does not finish
		// before the reading goroutine has had a chance to race anything.
		if err := stream.Send(&pingv1.CumSumResponse{Sum: request.Number}); err != nil {
			return err
		}
	}
}

// Reading request metadata from one goroutine while another receives must be
// safe. It is only safe because nothing writes CallInfo once the handler
// exists: the opening message is consumed before dispatch, and a second one
// is a protocol error. Run under -race, where this fails if either half of
// that regresses.
func TestRequestMetadataIsSafeForConcurrentReaders(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, racingServer{})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	metadata, err := json.Marshal(map[string][]string{"Acme-Tenant": {"tenant-42"}})
	assert.Nil(t, err)
	client := dialBrowserClientOpening(
		t, httpServer, pingv1connect.PingServiceCumSumProcedure, metadata)
	payload, err := proto.Marshal(&pingv1.CumSumRequest{Number: 1})
	assert.Nil(t, err)
	client.writeProto(t, payload)
	client.readMessage(t)
	client.writeJSON(t, wireClientEndStream, nil)
}

// A stream carries exactly one M message, and it must be the
// first message: the server consumes it before dispatching, so anything else
// arriving first means the peer is not speaking this protocol.
func TestStreamMustOpenWithLeadingMetadata(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, observationServer{
		observations: make(chan string, 4),
	})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	// A body message where the opening metadata belongs.
	conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
	sendProtoBody(t, conn, &pingv1.CumSumRequest{Number: 1})

	_, data, err := conn.Read(t.Context())
	assert.Nil(t, err)
	assert.Equal(t, rune(data[0]), wireServerEndStream) // S carries the verdict
	assert.True(t, strings.Contains(string(data), "invalid_argument"))
	assert.True(t, strings.Contains(string(data), "first message must be M"))
}

// dialRaw opens a connection without sending the opening message, so a test
// can put something else first.
func dialRaw(tb testing.TB, server *httptest.Server, procedure string) *websocket.Conn {
	tb.Helper()
	conn, response, err := websocket.Dial(
		tb.Context(),
		"ws"+strings.TrimPrefix(server.URL, "http")+procedure,
		&websocket.DialOptions{
			HTTPClient:   server.Client(),
			Subprotocols: []string{"connectrpc.1+proto"},
		},
	)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	assert.Nil(tb, err)
	tb.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// A key that arrived on the handshake is not overwritable from an M message.
// The precedence is a security property: a proxy sets headers on the upgrade
// and never sees the M, so the opposite order would let any client forge the
// identity headers such a proxy injects.
func TestHandshakeHeadersOutrankLeadingMetadata(t *testing.T) {
	t.Parallel()
	observations := make(chan string, 4)
	httpServer := newObservationServer(t, observations)

	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") +
		pingv1connect.PingServiceCumSumProcedure
	conn, response, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{
		HTTPClient:   httpServer.Client(),
		Subprotocols: []string{"connectrpc.1+proto"},
		// What a proxy in front of the server would have set.
		HTTPHeader: http.Header{"Acme-Tenant": []string{"from-the-proxy"}},
	})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	assert.Nil(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })

	// The client tries to claim a different tenant, and to add one the
	// handshake never carried.
	sendJSONMessage(t, conn, wireMetadata,
		[]byte(`{"Acme-Tenant":["forged"],"Acme-Trace":["added"]}`))
	sendProtoBody(t, conn, &pingv1.CumSumRequest{Number: 1})

	assert.Equal(t, <-observations, "from-the-proxy")
	_, _, err = conn.Read(t.Context())
	assert.Nil(t, err)
	assert.Equal(t, <-observations, "from-the-proxy")
}

// The other half of the same rule: a key the handshake did not carry is the
// client's to set, which is the only channel a browser has.
func TestLeadingMetadataAddsNewKeys(t *testing.T) {
	t.Parallel()
	observations := make(chan string, 4)
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, traceObservingServer{observations: observations})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
	sendJSONMessage(t, conn, wireMetadata, []byte(`{"Acme-Trace":["added"]}`))
	sendProtoBody(t, conn, &pingv1.CumSumRequest{Number: 1})

	assert.Equal(t, <-observations, "added")
}

// traceObservingServer reports a header the handshake never carried.
type traceObservingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	observations chan string
}

func (s traceObservingServer) CumSum(
	ctx context.Context,
	stream pingv1connect.PingServiceCumSumServerStream,
) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	s.observations <- info.RequestHeader().Get("Acme-Trace")
	for {
		if _, err := stream.Receive(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
