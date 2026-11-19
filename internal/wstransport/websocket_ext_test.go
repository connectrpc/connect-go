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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/wstransport"
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
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
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

// newLimitedServer mounts the service with an explicit read limit, so a test
// can carry a payload the default would refuse.
func newLimitedServer(tb testing.TB, readMaxBytes int) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, connecthttp.WithReadMaxBytes(readMaxBytes))
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
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
	transport := connecthttp.NewTransport(
		&http.Client{Transport: recorder},
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 1, Text: "hello"})
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
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
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
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
		connecthttp.WithSendCompression("gzip"),
		connecthttp.WithCompressMinBytes(1),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	text := strings.Repeat("compress me ", 1024)
	res, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 1, Text: text})
	assert.Nil(t, err)
	assert.Equal(t, res.Text, text)
}

// newCompressionServer mounts the service with the given options so a test can
// disable compression on the server alone.
func newCompressionServer(tb testing.TB, options ...connecthttp.Option) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, options...)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// negotiatedExtensions runs one RPC and reports what the handshake agreed.
func negotiatedExtensions(
	tb testing.TB,
	httpServer *httptest.Server,
	options ...connecthttp.Option,
) string {
	tb.Helper()
	recorder := &recordingTransport{base: httpServer.Client().Transport}
	transport := connecthttp.NewTransport(
		&http.Client{Transport: recorder},
		httpServer.URL,
		append([]connecthttp.Option{
			connecthttp.WithWebSocket(connecthttp.SelectAll),
		}, options...)...,
	)
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
	httpServer := newCompressionServer(t, connecthttp.WithoutWebSocketCompression())
	assert.Equal(t, negotiatedExtensions(t, httpServer), "")
}

// Disabling on the client must win even though the server still offers it.
func TestWithoutCompressionOnTheClient(t *testing.T) {
	t.Parallel()
	httpServer := newCompressionServer(t)
	assert.Equal(t, negotiatedExtensions(t, httpServer, connecthttp.WithoutWebSocketCompression()), "")
}

// TestCompressMinBytesAgreesAcrossHalves pins that the threshold means the same
// thing whichever wire an RPC takes. The two libraries read a zero differently
// — one as "use my default", the other as "no minimum" — so an unset or
// explicitly-zero option is exactly where they would drift apart.
func TestCompressMinBytesAgreesAcrossHalves(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		options []connecthttp.Option
	}{
		{"default", nil},
		{"explicit zero", []connecthttp.Option{connecthttp.WithCompressMinBytes(0)}},
		{"explicit value", []connecthttp.Option{connecthttp.WithCompressMinBytes(4096)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newCompressionServer(t, test.options...)
			transport := connecthttp.NewTransport(
				httpServer.Client(),
				httpServer.URL,
				append([]connecthttp.Option{
					connecthttp.WithWebSocket(connecthttp.SelectStreaming),
				}, test.options...)...,
			)
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
		{connect.CodecNameProto, "connectrpc.1+proto"},
		{connect.CodecNameJSON, "connectrpc.1+json"},
	} {
		t.Run(test.codec, func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer(t, pingServer{})
			recorder := &recordingTransport{base: httpServer.Client().Transport}
			transport := connecthttp.NewTransport(
				&http.Client{Transport: recorder},
				httpServer.URL,
				connecthttp.WithWebSocket(connecthttp.SelectAll),
				connecthttp.WithSendCodec(test.codec),
			)
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

// mountBoth registers server on mux over both transports, with WebSocket
// options the public connecthttp surface deliberately has no name for.
//
// Test scaffolding only. A user configures through connecthttp; these tests
// live beside the internal package and reach it directly so that behaviour
// with no public knob is still covered.
func mountBoth(mux *http.ServeMux, server *connect.Server, options ...wstransport.ServerOption) {
	connecthttp.Mount(&upgradingMux{mux: mux, server: server, options: options}, server,
		connecthttp.WithoutWebSocket())
}

type upgradingMux struct {
	mux     *http.ServeMux
	server  *connect.Server
	options []wstransport.ServerOption
}

func (m *upgradingMux) Handle(pattern string, handler http.Handler) {
	m.mux.Handle(pattern, wstransport.Upgrade(m.server, handler, m.options...))
}
