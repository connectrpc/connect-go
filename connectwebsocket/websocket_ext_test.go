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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

type pingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	// sawDeadline records whether CumSum's context carried a deadline, so a
	// test can tell that the client's timeout crossed the wire.
	sawDeadline chan time.Duration
}

func (pingServer) Ping(_ context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	return &pingv1.PingResponse{Number: req.Number, Text: req.Text}, nil
}

func (p pingServer) CumSum(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	if p.sawDeadline != nil {
		var remaining time.Duration
		if deadline, ok := ctx.Deadline(); ok {
			remaining = time.Until(deadline)
		}
		p.sawDeadline <- remaining
	}
	var total int64
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		total += req.Number
		if err := stream.Send(&pingv1.CumSumResponse{Sum: total}); err != nil {
			return err
		}
	}
}

// newHybridServer mounts the ping service so that each procedure answers both
// plain HTTP and a WebSocket upgrade on the same path.
func newHybridServer(tb testing.TB, handler pingServer) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, handler)
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

func newHybridClient(
	tb testing.TB,
	httpServer *httptest.Server,
	protocol *string,
) pingv1connect.PingServiceClient {
	tb.Helper()
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	assert.Nil(tb, err)
	return pingv1connect.NewPingServiceClient(
		connect.NewClient(transport, recordProtocol(protocol)),
	)
}

// recordProtocol captures the wire protocol the transport resolved, so a test
// can tell a WebSocket call from one that fell through to HTTP.
func recordProtocol(into *string) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			stream, err := next(ctx, spec)
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				*into = info.Protocol
			}
			return stream, err
		}
	}
}

func TestHybridUnaryOverHTTP(t *testing.T) {
	t.Parallel()
	var protocol string
	client := newHybridClient(t, newHybridServer(t, pingServer{}), &protocol)
	res, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 42, Text: "hello"})
	assert.Nil(t, err)
	assert.Equal(t, res.Number, int64(42))
	assert.Equal(t, res.Text, "hello")
	// Unary must not have been upgraded.
	assert.Equal(t, protocol, connect.ProtocolNameConnect)
}

func TestHybridBidiOverWebSocket(t *testing.T) {
	t.Parallel()
	var protocol string
	client := newHybridClient(t, newHybridServer(t, pingServer{}), &protocol)
	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	var got []int64
	for _, number := range []int64{1, 2, 3, 4} {
		assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: number}))
		res, err := stream.Receive()
		assert.Nil(t, err)
		got = append(got, res.Sum)
	}
	assert.Nil(t, stream.CloseSend())
	_, err = stream.Receive()
	assert.True(t, errors.Is(err, io.EOF))
	assert.Nil(t, stream.Close())
	assert.Equal(t, got, []int64{1, 3, 6, 10})
	assert.Equal(t, protocol, connectwebsocket.ProtocolConnectWebSocket)
}

func TestWebSocketCarriesDeadline(t *testing.T) {
	t.Parallel()
	deadlines := make(chan time.Duration, 1)
	var protocol string
	client := newHybridClient(t, newHybridServer(t, pingServer{sawDeadline: deadlines}), &protocol)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	_, err = stream.Receive()
	assert.Nil(t, err)

	remaining := <-deadlines
	assert.True(t, remaining > 0)
	// The client sends whole milliseconds, so the server's deadline is the
	// client's minus flight time, never more.
	assert.True(t, remaining <= time.Minute)
	// Generous lower bound: this only has to prove the deadline crossed the
	// wire, not that the clocks agree closely.
	assert.True(t, remaining > 30*time.Second)

	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())
	assert.Equal(t, protocol, connectwebsocket.ProtocolConnectWebSocket)
}

// A client with no deadline of its own inherits the server's default bound.
func TestWebSocketWithoutDeadline(t *testing.T) {
	t.Parallel()
	deadlines := make(chan time.Duration, 1)
	var protocol string
	client := newHybridClient(t, newHybridServer(t, pingServer{sawDeadline: deadlines}), &protocol)

	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	_, err = stream.Receive()
	assert.Nil(t, err)
	// t.Context() has no deadline, so the server's default applies.
	remaining := <-deadlines
	assert.True(t, remaining > 59*time.Minute)
	assert.True(t, remaining <= time.Hour)
	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())
}

// newLimitedServer mounts the service with a small read limit so a test can
// exceed it without allocating anything large.
func newLimitedServer(tb testing.TB, readMaxBytes int) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	options := []connectwebsocket.ServerOption{connectwebsocket.WithReadMaxBytes(readMaxBytes)}
	connecthttp.Mount(
		connectwebsocket.Mux(mux, server, options...),
		server,
	)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// TestWebSocketRejectsOversizeMessage sends a message past the server's limit.
// SelectAll puts the unary RPC on WebSocket, because it is the only ping
// message with a field big enough to exceed a limit with.
func TestWebSocketRejectsOversizeMessage(t *testing.T) {
	t.Parallel()
	httpServer := newLimitedServer(t, 128)
	// The client keeps the default limit, so the rejection has to come from the
	// server rather than from the sender refusing to send.
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{
		Number: 1,
		Text:   strings.Repeat("x", 4096),
	})
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
}

// TestWebSocketAcceptsMessageUnderLimit pins the other side of the boundary, so
// the rejection test cannot pass because everything fails.
func TestWebSocketAcceptsMessageUnderLimit(t *testing.T) {
	t.Parallel()
	httpServer := newLimitedServer(t, 4096)
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	text := strings.Repeat("x", 512)
	res, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 1, Text: text})
	assert.Nil(t, err)
	assert.Equal(t, res.Text, text)
}

// recordingTransport captures the upgrade response so a test can inspect what
// the handshake negotiated.
type recordingTransport struct {
	base        http.RoundTripper
	extensions  atomic.Pointer[string]
	subprotocol atomic.Pointer[string]
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	if res != nil {
		value := res.Header.Get("Sec-WebSocket-Extensions")
		t.extensions.Store(&value)
		negotiated := res.Header.Get("Sec-WebSocket-Protocol")
		t.subprotocol.Store(&negotiated)
	}
	return res, err
}

// TestWebSocketNegotiatesPerMessageDeflate pins the compression contract: the
// handshake must agree permessage-deflate, and it must be no-context-takeover
// in both directions. A shared compression context across messages is the
// CRIME/BREACH exposure the design forbids.
func TestWebSocketNegotiatesPerMessageDeflate(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	recorder := &recordingTransport{base: httpServer.Client().Transport}
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(&http.Client{Transport: recorder}),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err = client.Ping(t.Context(), &pingv1.PingRequest{Number: 1, Text: "hello"})
	assert.Nil(t, err)

	negotiated := recorder.extensions.Load()
	assert.NotNil(t, negotiated)
	assert.True(t, strings.Contains(*negotiated, "permessage-deflate"))
	assert.True(t, strings.Contains(*negotiated, "client_no_context_takeover"))
	assert.True(t, strings.Contains(*negotiated, "server_no_context_takeover"))
}

// TestWebSocketRoundTripsCompressiblePayload exercises a payload large enough
// to actually be deflated, so the compression path is not merely negotiated.
func TestWebSocketRoundTripsCompressiblePayload(t *testing.T) {
	t.Parallel()
	httpServer := newLimitedServer(t, 1024*1024)
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	text := strings.Repeat("compress me ", 8192) // ~96KiB, highly compressible
	res, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 7, Text: text})
	assert.Nil(t, err)
	assert.Equal(t, res.Text, text)
	assert.Equal(t, res.Number, int64(7))
}

// TestWebSocketIgnoresSendCompression pins that a Connect-level send
// compressor does not reach the WebSocket path. The peer rejects a
// compressed-message flag outright, so if this option ever leaked through
// again the RPC would fail rather than merely double-compress.
func TestWebSocketIgnoresSendCompression(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
		connectwebsocket.WithSendCompression("gzip"),
		connectwebsocket.WithCompressMinBytes(1),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	text := strings.Repeat("compress me ", 1024)
	res, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 1, Text: text})
	assert.Nil(t, err)
	assert.Equal(t, res.Text, text)
}

// newCompressionServer mounts the service with the given options so a test can
// disable compression on the server alone.
func newCompressionServer(tb testing.TB, options ...connectwebsocket.ServerOption) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(
		connectwebsocket.Mux(mux, server, options...),
		server,
	)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// negotiatedExtensions runs one RPC and reports what the handshake agreed.
func negotiatedExtensions(
	tb testing.TB,
	httpServer *httptest.Server,
	options ...connectwebsocket.ClientOption,
) string {
	tb.Helper()
	recorder := &recordingTransport{base: httpServer.Client().Transport}
	options = append(options,
		connectwebsocket.WithHTTPClient(&http.Client{Transport: recorder}),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	transport, err := connectwebsocket.NewTransport(httpServer.URL, options...)
	assert.Nil(tb, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	text := strings.Repeat("compress me ", 512)
	res, err := client.Ping(tb.Context(), &pingv1.PingRequest{Number: 1, Text: text})
	assert.Nil(tb, err)
	// The RPC must still work either way; only the framing changes.
	assert.Equal(tb, res.Text, text)

	negotiated := recorder.extensions.Load()
	assert.NotNil(tb, negotiated)
	return *negotiated
}

// Disabling on the server must win even though the client still offers it.
func TestWithoutCompressionOnTheServer(t *testing.T) {
	t.Parallel()
	httpServer := newCompressionServer(t, connectwebsocket.WithoutCompression())
	assert.Equal(t, negotiatedExtensions(t, httpServer), "")
}

// Disabling on the client must win even though the server still offers it.
func TestWithoutCompressionOnTheClient(t *testing.T) {
	t.Parallel()
	httpServer := newCompressionServer(t)
	assert.Equal(t, negotiatedExtensions(t, httpServer, connectwebsocket.WithoutCompression()), "")
}

// With neither side disabling, compression is still negotiated — otherwise the
// two tests above could pass for the wrong reason.
func TestCompressionStaysOnByDefault(t *testing.T) {
	t.Parallel()
	httpServer := newCompressionServer(t)
	negotiated := negotiatedExtensions(t, httpServer)
	assert.True(t, strings.Contains(negotiated, "permessage-deflate"))
	assert.True(t, strings.Contains(negotiated, "client_no_context_takeover"))
}

// TestCompressMinBytesAgreesAcrossHalves pins that the threshold means the same
// thing whichever wire an RPC takes. The two libraries read a zero differently
// — one as "use my default", the other as "no minimum" — so an unset or
// explicitly-zero option is exactly where they would drift apart.
func TestCompressMinBytesAgreesAcrossHalves(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		options []connectwebsocket.Option
	}{
		{"default", nil},
		{"explicit zero", []connectwebsocket.Option{connectwebsocket.WithCompressMinBytes(0)}},
		{"explicit value", []connectwebsocket.Option{connectwebsocket.WithCompressMinBytes(4096)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newCompressionServer(t, asServerOptions(test.options)...)
			transport, err := connectwebsocket.NewTransport(
				httpServer.URL,
				append([]connectwebsocket.ClientOption{
					connectwebsocket.WithHTTPClient(httpServer.Client()),
				}, asClientOptions(test.options)...)...,
			)
			assert.Nil(t, err)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

			// Well under the default threshold, and well over it, over both
			// wires: unary falls through to HTTP, CumSum goes over WebSocket.
			for _, text := range []string{"tiny", strings.Repeat("x", 8192)} {
				res, err := client.Ping(t.Context(), &pingv1.PingRequest{Text: text})
				assert.Nil(t, err)
				assert.Equal(t, res.Text, text)
			}
			stream, err := client.CumSum(t.Context())
			assert.Nil(t, err)
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 5}))
			sum, err := stream.Receive()
			assert.Nil(t, err)
			assert.Equal(t, sum.Sum, int64(5))
			assert.Nil(t, stream.CloseSend())
			assert.Nil(t, stream.Close())
		})
	}
}

// TestSendCodecSelectsSubprotocol pins that the codec and the negotiated
// subprotocol cannot disagree. They are chosen in different places — the codec
// from WithSendCodec, the token in the dial options — so a mismatch encodes
// with one and decodes with the other, which surfaces as a corrupt-wire-format
// error rather than anything pointing at the codec.
func TestSendCodecSelectsSubprotocol(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		codec string
		token string
	}{
		{connect.CodecNameProto, "connect.v2+proto"},
		{connect.CodecNameJSON, "connect.v2+json"},
	} {
		t.Run(test.codec, func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer(t, pingServer{})
			recorder := &recordingTransport{base: httpServer.Client().Transport}
			transport, err := connectwebsocket.NewTransport(
				httpServer.URL,
				connectwebsocket.WithHTTPClient(&http.Client{Transport: recorder}),
				connectwebsocket.WithSelector(connectwebsocket.SelectAll),
				connectwebsocket.WithSendCodec(test.codec),
			)
			assert.Nil(t, err)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

			res, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 42, Text: "hello"})
			assert.Nil(t, err)
			assert.Equal(t, res.Number, int64(42))
			assert.Equal(t, res.Text, "hello")
			assert.Equal(t, recorder.subprotocol.Load() != nil, true)
			assert.Equal(t, *recorder.subprotocol.Load(), test.token)
		})
	}
}

// asClientOptions and asServerOptions widen a shared option list to one side.
// A []Option is not assignable to []ClientOption in Go even though each element
// is, so a test that deliberately configures both halves identically has to
// convert.
func asClientOptions(options []connectwebsocket.Option) []connectwebsocket.ClientOption {
	converted := make([]connectwebsocket.ClientOption, 0, len(options))
	for _, option := range options {
		converted = append(converted, option)
	}
	return converted
}

func asServerOptions(options []connectwebsocket.Option) []connectwebsocket.ServerOption {
	converted := make([]connectwebsocket.ServerOption, 0, len(options))
	for _, option := range options {
		converted = append(converted, option)
	}
	return converted
}
