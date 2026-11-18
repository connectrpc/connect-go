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

package wstransport_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// newFailingClient points a WebSocket-only client at a server that answers
// every request with status, so the handshake fails in a controlled way.
// SelectAll keeps the unary call off the HTTP fallback.
func newFailingClient(tb testing.TB, status int) pingv1connect.PingServiceClient {
	tb.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	tb.Cleanup(server.Close)
	transport := connecthttp.NewTransport(
		server.Client(),
		server.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	return pingv1connect.NewPingServiceClient(connect.NewClient(transport))
}

// A failed upgrade must carry the HTTP status through to a Connect code, so a
// caller can tell "no such method" from "try again later".
func TestDialFailureMapsHTTPStatus(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		status int
		want   connect.Code
	}{
		{http.StatusBadRequest, connect.CodeInternal},            // 400
		{http.StatusUnauthorized, connect.CodeUnauthenticated},   // 401
		{http.StatusForbidden, connect.CodePermissionDenied},     // 403
		{http.StatusNotFound, connect.CodeUnimplemented},         // 404
		{http.StatusTooManyRequests, connect.CodeUnavailable},    // 429
		{http.StatusBadGateway, connect.CodeUnavailable},         // 502
		{http.StatusServiceUnavailable, connect.CodeUnavailable}, // 503
		{http.StatusGatewayTimeout, connect.CodeUnavailable},     // 504
		{http.StatusTeapot, connect.CodeUnknown},                 // anything else
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			t.Parallel()
			client := newFailingClient(t, test.status)
			_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
			assert.NotNil(t, err)
			assert.Equal(t, connect.CodeOf(err), test.want)
		})
	}
}

// No server at all: there is no response to read a status from, so the code
// comes from the dial failure itself.
func TestDialFailureUnreachableServer(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	httpClient := server.Client()
	server.Close() // nothing is listening from here on

	transport := connecthttp.NewTransport(
		httpClient,
		url,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable)
}

// Cancellation is the caller's doing, so it must not be reported as a
// transport failure a caller might retry.
func TestDialFailureCanceledContext(t *testing.T) {
	t.Parallel()
	client := newFailingClient(t, http.StatusOK)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 1})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
}

func TestDialFailureDeadlineExceeded(t *testing.T) {
	t.Parallel()
	client := newFailingClient(t, http.StatusOK)
	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // make sure it really has expired
	_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 1})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded)
}

// A server that drops the connection mid-handshake leaves io.EOF at the bottom
// of the dial error. Callers read io.EOF as a clean end of stream, so a
// receive loop would take the failed call for an empty one that succeeded.
func TestDialFailureDroppedConnectionIsNotEndOfStream(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		if conn, _, err := hijacker.Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(server.Close)
	transport := connecthttp.NewTransport(
		server.Client(),
		server.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	// The dial runs on the opening Send, so the error may come from the call
	// itself or, if a transport defers it, from the first Receive.
	stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 1})
	if err == nil {
		_, err = stream.Receive()
		_ = stream.Close()
	}
	assert.False(t, errors.Is(err, io.EOF), assert.Sprintf("%v", err))
	assert.True(t, errors.Is(err, io.ErrUnexpectedEOF), assert.Sprintf("%v", err))
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable)

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable)
}
