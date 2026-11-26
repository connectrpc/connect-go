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

package connecthttp_test

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/memhttp"
	"connectrpc.com/connect/v2/internal/memhttp/memhttptest"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

func TestWebSocketRoundTrip(t *testing.T) {
	t.Parallel()
	sharedServer := newWebSocketServer(t, pingServer{checkMetadata: true})
	server := newWebSocketServer(t, &websocketPingServer{pingServer: pingServer{includeErrorDetails: true}})
	for _, codec := range []string{connect.CodecNameProto, connect.CodecNameJSON} {
		t.Run(codec, func(t *testing.T) {
			t.Parallel()
			newClient := func(server *memhttp.Server, options ...connecthttp.Option) *connect.Client {
				options = append(options, connecthttp.WithWebSocket(), connecthttp.WithSendCodec(codec))
				return connect.NewClient(connecthttp.NewTransport(
					&http.Client{Transport: server.TransportHTTP1()}, server.URL(), options...,
				))
			}
			sharedClient := pingv1connect.NewPingServiceClient(newClient(sharedServer))
			connectClient := newClient(server)
			client := pingv1connect.NewPingServiceClient(connectClient)
			t.Run("unary", func(t *testing.T) {
				t.Parallel()
				testUnary(t, sharedClient)
			})
			t.Run("client_stream", func(t *testing.T) {
				t.Parallel()
				testClientStream(t, sharedClient)
			})
			t.Run("server_stream", func(t *testing.T) {
				t.Parallel()
				testServerStream(t, sharedClient)
			})
			t.Run("bidi", func(t *testing.T) {
				t.Parallel()
				testBidiStream(t, sharedClient, true)
			})
			t.Run("call_info", func(t *testing.T) {
				t.Parallel()
				ctx, info := connect.NewClientContext(t.Context())
				_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 7, Text: "hello"})
				assert.Nil(t, err)
				assert.Equal(t, info.Protocol, "connect-websocket")
				assert.Equal(t, info.Codec, codec)
				assert.Equal(t, info.ResponseEncoding, connect.CompressionNameIdentity)
				clientInfo, ok := connecthttp.ClientInfoForContext(ctx)
				assert.True(t, ok)
				assert.Equal(t, clientInfo.HTTPMethod(), http.MethodGet)
				assert.Equal(t, clientInfo.ResponseStatus(), http.StatusSwitchingProtocols)
			})
			t.Run("error", func(t *testing.T) {
				t.Parallel()
				_, err := client.Fail(t.Context(), &pingv1.FailRequest{Code: int32(connect.CodeResourceExhausted)})
				var connectErr *connect.Error
				assert.True(t, errors.As(err, &connectErr))
				assert.Equal(t, connectErr.Code(), connect.CodeResourceExhausted)
				assert.Equal(t, connectErr.Message(), "oh no")
				assert.Equal(t, len(connectErr.Details()), 1)
			})
			t.Run("deadline", func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				_, err := client.Ping(ctx, &pingv1.PingRequest{Text: "sleep"})
				assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded)
			})
			t.Run("send_without_close_send", func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				spec := connect.Spec{StreamType: connect.StreamTypeServer, Procedure: pingv1connect.PingServiceCountUpProcedure}
				stream, err := connectClient.CallClientStream(ctx, spec)
				assert.Nil(t, err)
				assert.Nil(t, stream.Send(&pingv1.CountUpRequest{Number: 1}))
				var response pingv1.CountUpResponse
				assert.Nil(t, stream.Receive(&response))
				assert.Equal(t, response.GetNumber(), 1)
			})
			t.Run("send_max_bytes", func(t *testing.T) {
				t.Parallel()
				client := pingv1connect.NewPingServiceClient(newClient(server, connecthttp.WithSendMaxBytes(4)))
				_, err := client.Ping(t.Context(), &pingv1.PingRequest{Text: "too long"})
				assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
			})
			t.Run("cancel_before_close_send", func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithCancel(t.Context())
				stream, err := client.Sum(ctx)
				assert.Nil(t, err)
				assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 1}))
				cancel()
				_, err = stream.CloseAndReceive()
				assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
			})
		})
	}
}

func TestWebSocketWire(t *testing.T) {
	t.Parallel()
	server := newWebSocketServer(t, &websocketPingServer{})
	limitedServer := newWebSocketServer(t, &websocketPingServer{}, connecthttp.WithSendMaxBytes(8), connecthttp.WithReadMaxBytes(1024))
	readLimitServer := newWebSocketServer(t, &websocketPingServer{}, connecthttp.WithReadMaxBytes(64))
	ignoreSendErrorServer := newWebSocketServer(t, &pluggablePingServer{
		countUp: func(ctx context.Context, request *pingv1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
			info, _ := connect.CallInfoForServerContext(ctx)
			info.ResponseHeader().Set("Connect-Custom", "1")
			for i := range request.GetNumber() {
				_ = stream.Send(&pingv1.CountUpResponse{Number: i + 1})
			}
			return nil
		},
	})
	ignoreDecodeErrorServer := newWebSocketServer(t, &pluggablePingServer{
		sum: func(_ context.Context, stream pingv1connect.PingServiceSumServerStream) (*pingv1.SumResponse, error) {
			_, _ = stream.Receive()
			return &pingv1.SumResponse{}, nil
		},
		cumSum: func(_ context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
			_, _ = stream.Receive()
			return nil
		},
	})
	otherErrorServer := newWebSocketServer(t, &pluggablePingServer{
		sum: func(_ context.Context, stream pingv1connect.PingServiceSumServerStream) (*pingv1.SumResponse, error) {
			_, _ = stream.Receive()
			return nil, connect.NewError(connect.CodeAborted, "other")
		},
		cumSum: func(_ context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
			_, _ = stream.Receive()
			return connect.NewError(connect.CodeAborted, "other")
		},
	})
	sendHeadersServer := newWebSocketServer(t, &pluggablePingServer{
		countUp: func(
			ctx context.Context,
			request *pingv1.CountUpRequest,
			stream pingv1connect.PingServiceCountUpServerStream,
		) error {
			info, _ := connect.CallInfoForServerContext(ctx)
			info.ResponseHeader().Set("X-B", "2")
			info.ResponseHeader().Set("X-A", "1")
			if err := stream.SendHeaders(); err != nil {
				return err
			}
			info.ResponseHeader().Set("X-Late", "1")
			for i := range request.GetNumber() {
				if err := stream.Send(&pingv1.CountUpResponse{Number: i + 1}); err != nil {
					return err
				}
			}
			return nil
		},
		cumSum: func(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
			<-ctx.Done()
			info, _ := connect.CallInfoForServerContext(ctx)
			info.ResponseHeader().Set("X-Send-Headers", connect.CodeOf(stream.SendHeaders()).String())
			return nil
		},
	})
	bigTrailer := strings.Repeat("a", 32)
	endStreamLimitServer := newWebSocketServer(t, &pluggablePingServer{
		ping: func(ctx context.Context, _ *pingv1.PingRequest) (*pingv1.PingResponse, error) {
			info, _ := connect.CallInfoForServerContext(ctx)
			info.ResponseTrailer().Set("X-Big", bigTrailer)
			detail, err := connectproto.NewErrorDetail(&pingv1.FailRequest{})
			if err != nil {
				return nil, err
			}
			return nil, connect.NewError(connect.CodeAborted, "failed").WithDetail(detail)
		},
		cumSum: func(ctx context.Context, _ pingv1connect.PingServiceCumSumServerStream) error {
			info, _ := connect.CallInfoForServerContext(ctx)
			info.ResponseTrailer().Set("X-Big", bigTrailer)
			return nil
		},
	}, connecthttp.WithSendMaxBytes(32))
	receiveAfterEndServer := newWebSocketServerWith(t, connect.NewServer(
		func(next connect.ServerFunc) connect.ServerFunc {
			return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
				if err := next(ctx, spec, stream); err != nil {
					return err
				}
				if err := stream.Receive(&pingv1.PingRequest{}); !errors.Is(err, io.EOF) {
					return connect.Errorf(connect.CodeInternal, "Receive after end of stream returned %v", err)
				}
				return nil
			}
		},
	), &websocketPingServer{})
	pingRequest := `{"number":"7"}`
	decodeError := func(cause string) websocketFrame {
		message, err := json.Marshal("protocol error: unmarshal message: " + cause)
		assert.Nil(t, err)
		return textFrame(`S{"error":{"code":"invalid_argument","message":` + string(message) + `}}`)
	}
	jsonDecodeError := decodeError("zero-length payload is not a valid JSON object")
	// protobuf-go varies the spacing in its error messages between builds.
	protoDecodeError := decodeError(proto.Unmarshal([]byte{0xff}, &pingv1.SumRequest{}).Error())
	tests := []struct {
		name        string
		server      *memhttp.Server
		procedure   string
		subprotocol string
		query       string
		header      http.Header
		send        []websocketFrame
		want        []websocketFrame
		wantStatus  websocket.StatusCode
	}{
		{
			name: "json",
			send: []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("S{}")},
		},
		{
			name:        "proto",
			subprotocol: "connectrpc.1+proto",
			send:        []websocketFrame{textFrame("M[]"), binaryFrame("B\x08\x07"), textFrame("C")},
			want:        []websocketFrame{textFrame("M[]"), binaryFrame("B\x08\x07"), textFrame("S{}")},
		},
		{
			name: "unary_two_bodies",
			send: []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("B" + pingRequest)},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"unimplemented","message":"unary request has multiple messages"}}`),
			},
		},
		{
			name: "unary_no_body",
			send: []websocketFrame{textFrame("M[]"), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"unimplemented","message":"unary request has zero messages"}}`),
			},
		},
		{
			name:      "server_stream_two_bodies",
			procedure: pingv1connect.PingServiceCountUpProcedure,
			send: []websocketFrame{
				textFrame("M[]"),
				textFrame(`B{"number":"1"}`),
				textFrame(`B{"number":"1"}`),
			},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"unimplemented","message":"unary request has multiple messages"}}`),
			},
		},
		{
			name:      "server_stream_no_body",
			procedure: pingv1connect.PingServiceCountUpProcedure,
			send:      []websocketFrame{textFrame("M[]"), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"unimplemented","message":"unary request has zero messages"}}`),
			},
		},
		{
			name:       "invalid_utf8",
			send:       []websocketFrame{textFrame("M\xff")},
			wantStatus: websocket.StatusInvalidFramePayloadData,
		},
		{
			name: "invalid_response_metadata",
			send: []websocketFrame{textFrame("M[]"), textFrame(`B{"text":"bad-metadata"}`), textFrame("C")},
			want: []websocketFrame{
				textFrame(`M[["x-ok","1"]]`),
				textFrame(`B{"text":"bad-metadata"}`),
				textFrame(`S{"metadata":{"x-ok":["1"]}}`),
			},
		},
		{
			name: "empty_json_body",
			send: []websocketFrame{textFrame("M[]"), textFrame("B"), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"invalid_argument","message":"protocol error: unmarshal message: zero-length payload is not a valid JSON object"}}`),
			},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name: "connect_response_header",
			send: []websocketFrame{textFrame("M[]"), textFrame(`B{"text":"connect-header"}`), textFrame("C")},
			want: []websocketFrame{
				textFrame(`M[["x-ok","1"]]`),
				textFrame(`S{"error":{"code":"internal","message":"response header \"connect-custom\" is reserved for the protocol"}}`),
			},
		},
		{
			name:      "connect_response_header_send_error_ignored",
			server:    ignoreSendErrorServer,
			procedure: pingv1connect.PingServiceCountUpProcedure,
			send:      []websocketFrame{textFrame("M[]"), textFrame(`B{"number":"2"}`), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"internal","message":"response header \"connect-custom\" is reserved for the protocol"}}`),
			},
		},
		{
			name:      "connect_response_header_no_messages",
			server:    ignoreSendErrorServer,
			procedure: pingv1connect.PingServiceCountUpProcedure,
			send:      []websocketFrame{textFrame("M[]"), textFrame(`B{}`), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"internal",` +
					`"message":"response header \"connect-custom\" is reserved for the protocol"}}`),
			},
		},
		{
			name: "empty_response_metadata",
			send: []websocketFrame{textFrame("M[]"), textFrame(`B{"text":"empty-metadata"}`), textFrame("C")},
			want: []websocketFrame{textFrame("M[]"), textFrame(`B{"text":"empty-metadata"}`), textFrame("S{}")},
		},
		{
			name:      "send_headers",
			server:    sendHeadersServer,
			procedure: pingv1connect.PingServiceCountUpProcedure,
			send:      []websocketFrame{textFrame("M[]"), textFrame(`B{"number":"2"}`), textFrame("C")},
			want: []websocketFrame{
				textFrame(`M[["x-a","1"],["x-b","2"]]`),
				textFrame(`B{"number":"1"}`),
				textFrame(`B{"number":"2"}`),
				textFrame("S{}"),
			},
		},
		{
			name:      "send_headers_after_deadline",
			server:    sendHeadersServer,
			procedure: pingv1connect.PingServiceCumSumProcedure,
			query:     "?connect-timeout-ms=10",
			send:      []websocketFrame{textFrame("M[]"), textFrame("C")},
			want: []websocketFrame{
				textFrame(`M[["x-send-headers","deadline_exceeded"]]`),
				textFrame(`S{"error":{"code":"deadline_exceeded","message":"context deadline exceeded"}}`),
			},
		},
		{
			name:      "end_stream_over_send_max_bytes",
			server:    endStreamLimitServer,
			procedure: pingv1connect.PingServiceCumSumProcedure,
			send:      []websocketFrame{textFrame("M[]"), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"resource_exhausted",` +
					`"message":"end of stream size 59 exceeds sendMaxBytes 32"}}`),
			},
		},
		{
			name:   "end_stream_error_over_send_max_bytes",
			server: endStreamLimitServer,
			send:   []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("C")},
			want:   []websocketFrame{textFrame("M[]"), textFrame(`S{"error":{"code":"aborted","message":"failed"}}`)},
		},
		{
			name:   "receive_after_end_unary",
			server: receiveAfterEndServer,
			send:   []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("C")},
			want:   []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("S{}")},
		},
		{
			name:      "receive_after_end_bidi",
			server:    receiveAfterEndServer,
			procedure: pingv1connect.PingServiceCumSumProcedure,
			send:      []websocketFrame{textFrame("M[]"), textFrame(`B{"number":"1"}`), textFrame("C")},
			want:      []websocketFrame{textFrame("M[]"), textFrame(`B{"sum":"1"}`), textFrame("S{}")},
		},
		{
			name: "end_stream_before_metadata",
			send: []websocketFrame{textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"internal","message":"protocol error: first message is C, expected M"}}`),
			},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name: "connect_response_trailer",
			send: []websocketFrame{textFrame("M[]"), textFrame(`B{"text":"connect-trailer"}`), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`B{"text":"connect-trailer"}`),
				textFrame(`S{"error":{"code":"internal","message":"response trailer \"connect-custom\" is reserved for the protocol"},"metadata":{"x-ok":["1"]}}`),
			},
		},
		{
			name:   "send_max_bytes",
			server: limitedServer,
			send:   []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"resource_exhausted","message":"message size 14 exceeds sendMaxBytes 8"}}`),
			},
		},
		{
			name: "missing_metadata",
			send: []websocketFrame{textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"internal","message":"protocol error: first message is B, expected M"}}`),
			},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name: "reserved_metadata",
			send: []websocketFrame{textFrame(`M[["x-forwarded-for","10.0.0.1"]]`), textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"internal","message":"protocol error: M message key \"x-forwarded-for\" is reserved"}}`),
			},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name:      "end_stream_from_client",
			procedure: pingv1connect.PingServiceCumSumProcedure,
			send:      []websocketFrame{textFrame("M[]"), textFrame("S{}")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"internal","message":"protocol error: unexpected S message from client"}}`),
			},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name: "message_after_end_stream",
			send: []websocketFrame{textFrame("M[]"), textFrame(`B{"text":"sleep"}`), textFrame("C"), textFrame(`B{}`)},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"internal","message":"protocol error: B message after C"}}`),
			},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name:   "metadata_matches_handshake_header",
			header: http.Header{"X-Test": {"handshake"}},
			send:   []websocketFrame{textFrame(`M[["x-test","metadata"]]`), textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"internal","message":"protocol error: M message key \"x-test\" is also a handshake header"}}`),
			},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name:   "metadata_combined_with_handshake",
			header: http.Header{"X-Other": {"handshake"}},
			send:   []websocketFrame{textFrame(`M[["x-test","metadata"]]`), textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame(`M[["x-test","metadata"]]`),
				textFrame("B" + pingRequest),
				textFrame(`S{"metadata":{"x-test":["metadata"]}}`),
			},
		},
		{
			name:  "deadline_before_metadata",
			query: "?connect-timeout-ms=50",
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"deadline_exceeded","message":"context deadline exceeded"}}`),
			},
		},
		{
			name:  "zero_timeout",
			query: "?connect-timeout-ms=0",
			send:  []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"deadline_exceeded","message":"context deadline exceeded"}}`),
			},
		},
		{
			name:  "malformed_timeout",
			query: "?connect-timeout-ms=1%zz",
			send:  []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"invalid_argument","message":"parse timeout: invalid URL escape \"%zz\""}}`),
			},
		},
		{
			name:       "undecodable_sum_json",
			procedure:  pingv1connect.PingServiceSumProcedure,
			send:       []websocketFrame{textFrame("M[]"), textFrame("B"), textFrame("C")},
			want:       []websocketFrame{textFrame("M[]"), jsonDecodeError},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name:        "undecodable_sum_proto",
			procedure:   pingv1connect.PingServiceSumProcedure,
			subprotocol: "connectrpc.1+proto",
			send:        []websocketFrame{textFrame("M[]"), binaryFrame("B\xff"), textFrame("C")},
			want:        []websocketFrame{textFrame("M[]"), protoDecodeError},
			wantStatus:  websocket.StatusProtocolError,
		},
		{
			name:       "undecodable_sum_ignored",
			server:     ignoreDecodeErrorServer,
			procedure:  pingv1connect.PingServiceSumProcedure,
			send:       []websocketFrame{textFrame("M[]"), textFrame("B"), textFrame("C")},
			want:       []websocketFrame{textFrame("M[]"), jsonDecodeError},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name:       "undecodable_sum_other_error",
			server:     otherErrorServer,
			procedure:  pingv1connect.PingServiceSumProcedure,
			send:       []websocketFrame{textFrame("M[]"), textFrame("B"), textFrame("C")},
			want:       []websocketFrame{textFrame("M[]"), jsonDecodeError},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name:       "undecodable_cumsum_json",
			procedure:  pingv1connect.PingServiceCumSumProcedure,
			send:       []websocketFrame{textFrame("M[]"), textFrame("B"), textFrame("C")},
			want:       []websocketFrame{textFrame("M[]"), jsonDecodeError},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name:        "undecodable_cumsum_proto",
			procedure:   pingv1connect.PingServiceCumSumProcedure,
			subprotocol: "connectrpc.1+proto",
			send:        []websocketFrame{textFrame("M[]"), binaryFrame("B\xff"), textFrame("C")},
			want:        []websocketFrame{textFrame("M[]"), protoDecodeError},
			wantStatus:  websocket.StatusProtocolError,
		},
		{
			name:       "undecodable_cumsum_ignored",
			server:     ignoreDecodeErrorServer,
			procedure:  pingv1connect.PingServiceCumSumProcedure,
			send:       []websocketFrame{textFrame("M[]"), textFrame("B"), textFrame("C")},
			want:       []websocketFrame{textFrame("M[]"), jsonDecodeError},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name:       "undecodable_cumsum_other_error",
			server:     otherErrorServer,
			procedure:  pingv1connect.PingServiceCumSumProcedure,
			send:       []websocketFrame{textFrame("M[]"), textFrame("B"), textFrame("C")},
			want:       []websocketFrame{textFrame("M[]"), jsonDecodeError},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name:        "read_max_bytes",
			server:      limitedServer,
			procedure:   pingv1connect.PingServiceCumSumProcedure,
			subprotocol: "connectrpc.1+proto",
			send:        []websocketFrame{textFrame("M[]"), binaryFrame("B" + strings.Repeat("\x00", 2048))},
			wantStatus:  websocket.StatusMessageTooBig,
		},
		{
			name:   "read_max_bytes_at_limit",
			server: readLimitServer,
			send: []websocketFrame{
				textFrame("M[]"),
				textFrame("B" + pingRequest + strings.Repeat(" ", 50)),
				textFrame("C"),
			},
			want: []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("S{}")},
		},
		{
			name:   "read_max_bytes_over_limit",
			server: readLimitServer,
			send: []websocketFrame{
				textFrame("M[]"),
				textFrame("B" + pingRequest + strings.Repeat(" ", 51)),
				textFrame("C"),
			},
			wantStatus: websocket.StatusMessageTooBig,
		},
		{
			name:        "read_max_bytes_at_limit_proto",
			server:      readLimitServer,
			subprotocol: "connectrpc.1+proto",
			send: []websocketFrame{
				textFrame("M[]"),
				binaryFrame("B\x12\x3e" + strings.Repeat("a", 62)),
				textFrame("C"),
			},
			want: []websocketFrame{
				textFrame("M[]"),
				binaryFrame("B\x12\x3e" + strings.Repeat("a", 62)),
				textFrame("S{}"),
			},
		},
		{
			name:        "read_max_bytes_over_limit_proto",
			server:      readLimitServer,
			subprotocol: "connectrpc.1+proto",
			send: []websocketFrame{
				textFrame("M[]"),
				binaryFrame("B\x12\x3f" + strings.Repeat("a", 63)),
				textFrame("C"),
			},
			wantStatus: websocket.StatusMessageTooBig,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			subprotocol := test.subprotocol
			if subprotocol == "" {
				subprotocol = "connectrpc.1+json"
			}
			wantStatus := test.wantStatus
			if wantStatus == 0 {
				wantStatus = websocket.StatusNormalClosure
			}
			testServer := server
			if test.server != nil {
				testServer = test.server
			}
			procedure := pingv1connect.PingServicePingProcedure
			if test.procedure != "" {
				procedure = test.procedure
			}
			conn := dialWebSocket(t, testServer, procedure+test.query, subprotocol, test.header)
			go func() { _ = writeFrames(ctx, conn, test.send...) }()
			got, closeErr := readFrames(ctx, conn)
			assert.Equal(t, got, test.want)
			assert.Equal(t, closeErr.Code, wantStatus)
		})
	}
}

func TestWebSocketClientProtocolErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		procedure       string
		skipNegotiation bool
		echo            string
		send            []websocketFrame
		drop            bool
		closeWith       websocket.StatusCode
		options         []connecthttp.Option
		wantOK          bool
		wantCode        connect.Code
		wantStatus      websocket.StatusCode
	}{
		{name: "no_subprotocol", skipNegotiation: true, wantStatus: -1},
		{name: "subprotocol_case", skipNegotiation: true, echo: "CONNECTRPC.1+PROTO", send: []websocketFrame{textFrame("M[]"), binaryFrame("B"), textFrame("S{}")}, wantStatus: -1},
		{name: "missing_metadata", send: []websocketFrame{binaryFrame("B")}, wantStatus: 3102},
		{name: "second_metadata", send: []websocketFrame{textFrame("M[]"), textFrame("M[]")}, wantStatus: 3102},
		{name: "client_end_stream", send: []websocketFrame{textFrame("M[]"), textFrame("C")}, wantStatus: 3102},
		{name: "null_end_stream", send: []websocketFrame{textFrame("M[]"), textFrame("Snull")}, wantStatus: 3102},
		{
			name:       "invalid_end_stream",
			send:       []websocketFrame{textFrame("M[]"), binaryFrame("B"), textFrame("S{")},
			wantStatus: 3102,
		},
		{name: "invalid_utf8", send: []websocketFrame{textFrame("M\xff")}, wantStatus: websocket.StatusInvalidFramePayloadData},
		{name: "long_reason", send: []websocketFrame{textFrame(`M[["` + strings.Repeat("X", 200) + `","v"]]`)}, wantStatus: 3102},
		{name: "invalid_trailer_key", send: []websocketFrame{textFrame("M[]"), binaryFrame("B"), textFrame(`S{"metadata":{"bad key":["1"]}}`)}, wantStatus: 3102},
		{name: "invalid_trailer_value", send: []websocketFrame{textFrame("M[]"), binaryFrame("B"), textFrame(`S{"metadata":{"x-test":["a\nb"]}}`)}, wantStatus: 3102},
		{
			name:       "empty_trailer_key",
			send:       []websocketFrame{textFrame("M[]"), binaryFrame("B"), textFrame(`S{"metadata":{"":["1"]}}`)},
			wantStatus: 3102,
		},
		{
			name: "binary_trailer_value",
			send: []websocketFrame{
				textFrame("M[]"),
				binaryFrame("B"),
				textFrame(`S{"metadata":{"x-test-bin":["a\nb"]}}`),
			},
			wantOK:     true,
			wantStatus: websocket.StatusNormalClosure,
		},
		{name: "unknown_marker", send: []websocketFrame{textFrame("M[]"), textFrame("X")}, wantStatus: 3102},
		{name: "two_bodies", send: []websocketFrame{textFrame("M[]"), binaryFrame("B"), binaryFrame("B"), textFrame("S{}")}, wantCode: connect.CodeUnimplemented, wantStatus: 3102},
		{name: "no_body", send: []websocketFrame{textFrame("M[]"), textFrame("S{}")}, wantCode: connect.CodeUnimplemented, wantStatus: websocket.StatusNormalClosure},
		{
			name:       "client_stream_two_bodies",
			procedure:  pingv1connect.PingServiceSumProcedure,
			send:       []websocketFrame{textFrame("M[]"), binaryFrame("B"), binaryFrame("B"), textFrame("S{}")},
			wantCode:   connect.CodeUnimplemented,
			wantStatus: 3102,
		},
		{
			name:       "client_stream_no_body",
			procedure:  pingv1connect.PingServiceSumProcedure,
			send:       []websocketFrame{textFrame("M[]"), textFrame("S{}")},
			wantCode:   connect.CodeUnimplemented,
			wantStatus: websocket.StatusNormalClosure,
		},
		{name: "message_too_big", send: []websocketFrame{textFrame("M[]")}, closeWith: websocket.StatusMessageTooBig, wantCode: connect.CodeResourceExhausted},
		{
			name:      "invalid_payload_close",
			send:      []websocketFrame{textFrame("M[]")},
			closeWith: websocket.StatusInvalidFramePayloadData,
		},
		{
			name: "read_max_bytes_at_limit",
			send: []websocketFrame{
				textFrame("M[]"),
				binaryFrame("B\x12\x0e" + strings.Repeat("a", 14)),
				textFrame("S{}"),
			},
			options:    []connecthttp.Option{connecthttp.WithReadMaxBytes(16)},
			wantOK:     true,
			wantStatus: websocket.StatusNormalClosure,
		},
		{
			name: "read_max_bytes_over_limit",
			send: []websocketFrame{
				textFrame("M[]"),
				binaryFrame("B\x12\x0f" + strings.Repeat("a", 15)),
				textFrame("S{}"),
			},
			options:    []connecthttp.Option{connecthttp.WithReadMaxBytes(16)},
			wantCode:   connect.CodeResourceExhausted,
			wantStatus: websocket.StatusMessageTooBig,
		},
		{
			name: "read_max_bytes_unlimited",
			send: []websocketFrame{
				textFrame("M[]"),
				// coder's default read limit is 32 KiB.
				binaryFrame("B\x12\x80\x80\x04" + strings.Repeat("a", 1<<16)),
				textFrame("S{}"),
			},
			options:    []connecthttp.Option{connecthttp.WithReadMaxBytes(0)},
			wantOK:     true,
			wantStatus: websocket.StatusNormalClosure,
		},
		{name: "undecodable_body", send: []websocketFrame{textFrame("M[]"), binaryFrame("B\xff")}, wantCode: connect.CodeInvalidArgument, wantStatus: 3102},
		{
			name:       "undecodable_body_server_stream",
			procedure:  pingv1connect.PingServiceCountUpProcedure,
			send:       []websocketFrame{textFrame("M[]"), binaryFrame("B\x08\x01"), binaryFrame("B\xff")},
			wantCode:   connect.CodeInvalidArgument,
			wantStatus: 3102,
		},
		{
			name:       "undecodable_body_bidi",
			procedure:  pingv1connect.PingServiceCumSumProcedure,
			send:       []websocketFrame{textFrame("M[]"), binaryFrame("B\x08\x01"), binaryFrame("B\xff")},
			wantCode:   connect.CodeInvalidArgument,
			wantStatus: 3102,
		},
		{name: "dropped_after_body", send: []websocketFrame{textFrame("M[]"), binaryFrame("B")}, drop: true, wantCode: connect.CodeUnavailable, wantStatus: -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			closeErr := make(chan websocket.CloseError, 1)
			server := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				options := &websocket.AcceptOptions{Subprotocols: []string{"connectrpc.1+proto"}}
				if test.skipNegotiation {
					// coder echoes the client's own spelling, so set the echo here.
					options = nil
					if test.echo != "" {
						response.Header().Set("Sec-WebSocket-Protocol", test.echo)
					}
				}
				conn, err := websocket.Accept(response, request, options)
				if err != nil {
					return
				}
				// net/http cancels the request context when the handler returns.
				ctx := context.WithoutCancel(request.Context())
				go func(ctx context.Context) {
					_, closeError := readFrames(ctx, conn)
					closeErr <- closeError
				}(ctx)
				_ = writeFrames(ctx, conn, test.send...)
				switch {
				case test.drop:
					_ = conn.CloseNow()
				case test.closeWith != 0:
					_ = conn.Close(test.closeWith, "")
				}
			}))
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
				&http.Client{Transport: server.TransportHTTP1()}, server.URL(),
				append(test.options, connecthttp.WithWebSocket())...,
			)))
			err := callWebSocketProcedure(t, client, test.procedure)
			wantCode := connect.CodeInternal
			if test.wantCode != 0 {
				wantCode = test.wantCode
			}
			if test.wantOK {
				assert.Nil(t, err)
			} else {
				assert.Equal(t, connect.CodeOf(err), wantCode)
			}
			if test.closeWith != 0 {
				return
			}
			closeError := receiveWithin(t, closeErr, "client close")
			assert.Equal(t, closeError.Code, test.wantStatus)
			if test.wantStatus == 3102 {
				assert.NotZero(t, closeError.Reason)
			}
		})
	}
}

// callWebSocketProcedure returns the first error from a call to procedure,
// which defaults to Ping. A stream must return the same error again.
func callWebSocketProcedure(t *testing.T, client pingv1connect.PingServiceClient, procedure string) error {
	t.Helper()
	switch procedure {
	case pingv1connect.PingServiceCountUpProcedure:
		stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 2})
		if err != nil {
			return err
		}
		defer stream.Close()
		for {
			if _, err := stream.Receive(); err != nil {
				_, again := stream.Receive()
				assert.ErrorIs(t, again, err)
				return err
			}
		}
	case pingv1connect.PingServiceSumProcedure:
		stream, err := client.Sum(t.Context())
		if err != nil {
			return err
		}
		_, err = stream.CloseAndReceive()
		return err
	case pingv1connect.PingServiceCumSumProcedure:
		stream, err := client.CumSum(t.Context())
		if err != nil {
			return err
		}
		defer stream.Close()
		if err := stream.Send(&pingv1.CumSumRequest{Number: 1}); err != nil {
			return err
		}
		for {
			if _, err := stream.Receive(); err != nil {
				_, again := stream.Receive()
				assert.ErrorIs(t, again, err)
				return err
			}
		}
	default:
		_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
		return err
	}
}

func TestWebSocketClientSilentServer(t *testing.T) {
	t.Parallel()
	silentServer := func(t *testing.T, reads int, send ...websocketFrame) connecthttp.HTTPClient {
		t.Helper()
		server := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			conn, err := websocket.Accept(response, request, &websocket.AcceptOptions{Subprotocols: []string{"connectrpc.1+proto"}})
			if err != nil {
				return
			}
			ctx := context.WithoutCancel(request.Context())
			for range reads {
				if _, _, err := conn.Read(ctx); err != nil {
					return
				}
			}
			_ = writeFrames(ctx, conn, send...)
		}))
		return &http.Client{Transport: server.TransportHTTP1()}
	}
	newClient := func(t *testing.T, httpClient connecthttp.HTTPClient) pingv1connect.PingServiceClient {
		t.Helper()
		return pingv1connect.NewPingServiceClient(connect.NewClient(
			connecthttp.NewTransport(httpClient, "http://silent", connecthttp.WithWebSocket()),
		))
	}
	t.Run("after_end_stream", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			client := newClient(t, silentServer(t, 3, textFrame("M[]"), binaryFrame("B"), textFrame("S{}")))
			start := time.Now()
			_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
			assert.Nil(t, err)
			assert.True(t, time.Since(start) < time.Second)
			// Outlast coder's 5s close handshake before the bubble ends.
			time.Sleep(10 * time.Second)
		})
	})
	// A second B fails the call without waiting for S.
	t.Run("unary_two_bodies", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			client := newClient(t, silentServer(t, 3, textFrame("M[]"), binaryFrame("B"), binaryFrame("B")))
			start := time.Now()
			_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
			assert.Equal(t, connect.CodeOf(err), connect.CodeUnimplemented)
			assert.True(t, time.Since(start) < time.Second)
			// Outlast coder's 5s close handshake before the bubble ends.
			time.Sleep(10 * time.Second)
		})
	})
	t.Run("client_stream_two_bodies", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			client := newClient(t, silentServer(t, 2, textFrame("M[]"), binaryFrame("B"), binaryFrame("B")))
			stream, err := client.Sum(t.Context())
			assert.Nil(t, err)
			start := time.Now()
			_, err = stream.CloseAndReceive()
			assert.Equal(t, connect.CodeOf(err), connect.CodeUnimplemented)
			assert.True(t, time.Since(start) < time.Second)
			// Outlast coder's 5s close handshake before the bubble ends.
			time.Sleep(10 * time.Second)
		})
	})
	t.Run("close_while_receiving", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			client := newClient(t, silentServer(t, 1))
			stream, err := client.CumSum(t.Context())
			assert.Nil(t, err)
			assert.Nil(t, stream.SendHeaders())
			errs := make(chan error, 1)
			go func() {
				_, err := stream.Receive()
				errs <- err
			}()
			synctest.Wait()
			start := time.Now()
			_ = stream.Close()
			assert.Equal(t, connect.CodeOf(<-errs), connect.CodeCanceled)
			assert.True(t, time.Since(start) < 3*time.Second)
			// Outlast coder's 5s close handshake before the bubble ends.
			time.Sleep(10 * time.Second)
		})
	})
}

func TestWebSocketNegotiation(t *testing.T) {
	t.Parallel()
	server := newWebSocketServer(t, &websocketPingServer{})
	mux := http.NewServeMux()
	notEnabled := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(notEnabled, &websocketPingServer{})
	connecthttp.Mount(mux, notEnabled)
	notEnabledServer := memhttptest.NewServer(t, mux)
	tests := []struct {
		name            string
		server          *memhttp.Server
		header          http.Header
		offer           []string
		wantSubprotocol string
		wantStatus      int
	}{
		{
			name:            "mixed_offer",
			server:          server,
			offer:           []string{"connectrpc.1+xml", "connectrpc.1+json"},
			wantSubprotocol: "connectrpc.1+json",
		},
		{
			name:            "other_subprotocol",
			server:          server,
			offer:           []string{"chat", "connectrpc.1+json"},
			wantSubprotocol: "connectrpc.1+json",
		},
		{
			name:       "cross_origin",
			server:     server,
			header:     http.Header{"Origin": {"http://evil.example"}},
			offer:      []string{"connectrpc.1+json"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "unsupported_subprotocol",
			server:     server,
			offer:      []string{"connectrpc.1+xml"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "not_enabled",
			server:     notEnabledServer,
			offer:      []string{"connectrpc.1+json"},
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// Bidi is the only procedure that requireHTTP2 also wraps.
			conn, response, err := websocket.Dial(t.Context(), test.server.URL()+pingv1connect.PingServiceCumSumProcedure, &websocket.DialOptions{
				HTTPClient:   &http.Client{Transport: test.server.TransportHTTP1()},
				HTTPHeader:   test.header,
				Subprotocols: test.offer,
			})
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if test.wantStatus != 0 {
				assert.NotNil(t, err)
				assert.Equal(t, response.StatusCode, test.wantStatus)
				return
			}
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			assert.Equal(t, conn.Subprotocol(), test.wantSubprotocol)
			assert.Nil(t, conn.CloseNow())
		})
	}
}

func TestWebSocketClientAfterEndStream(t *testing.T) {
	t.Parallel()
	received := make(chan []websocketFrame, 1)
	closeStatus := make(chan websocket.StatusCode, 1)
	server := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(response, request, &websocket.AcceptOptions{Subprotocols: []string{"connectrpc.1+proto"}})
		if err != nil {
			return
		}
		ctx := context.WithoutCancel(request.Context())
		go func() {
			frames, closeErr := readFrames(ctx, conn)
			received <- frames
			closeStatus <- closeErr.Code
		}()
		_ = writeFrames(ctx, conn, textFrame("M[]"), textFrame("S{}"))
	}))
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
		&http.Client{Transport: server.TransportHTTP1()}, server.URL(), connecthttp.WithWebSocket(),
	)))
	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.SendHeaders())
	_, err = stream.Receive()
	assert.ErrorIs(t, err, io.EOF)
	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())
	assert.Equal(t, receiveWithin(t, received, "client close"), []websocketFrame{textFrame("M[]")})
	assert.Equal(t, <-closeStatus, websocket.StatusNormalClosure)
}

func TestWebSocketClientHandshake(t *testing.T) {
	t.Parallel()
	handshake := func(t *testing.T, ctx context.Context) *http.Request {
		t.Helper()
		handshakes := make(chan *http.Request, 1)
		mux := http.NewServeMux()
		server := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(server, &websocketPingServer{})
		connecthttp.Mount(mux, server, connecthttp.WithWebSocket())
		httpServer := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			handshakes <- request.Clone(request.Context())
			mux.ServeHTTP(response, request)
		}))
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
			&http.Client{Transport: httpServer.TransportHTTP1()}, httpServer.URL(), connecthttp.WithWebSocket(),
		)))
		_, err := client.Ping(ctx, &pingv1.PingRequest{})
		assert.Nil(t, err)
		return <-handshakes
	}
	t.Run("default", func(t *testing.T) {
		t.Parallel()
		request := handshake(t, t.Context())
		userAgent := request.Header.Values("User-Agent")
		assert.Equal(t, len(userAgent), 1)
		assert.True(t, strings.HasPrefix(userAgent[0], "connect-go/"))
		// Spec Section 4.3: the subprotocol implies the protocol version.
		assert.Zero(t, request.Header.Get("Connect-Protocol-Version"))
		assert.Equal(t, request.URL.RawQuery, "")
	})
	t.Run("deadline", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		request := handshake(t, ctx)
		timeout, err := strconv.Atoi(request.URL.Query().Get("connect-timeout-ms"))
		assert.Nil(t, err)
		assert.True(t, timeout > 0 && timeout <= 60000, assert.Sprintf("timeout %d", timeout))
	})
}

func TestWebSocketHTTPClientWrapper(t *testing.T) {
	t.Parallel()
	server := newWebSocketServer(t, &websocketPingServer{})
	pass := &http.Client{Transport: server.TransportHTTP1()}
	// A Timeout makes net/http wrap the body, so it can't be written.
	withTimeout := &http.Client{Transport: server.TransportHTTP1(), Timeout: time.Minute}
	tests := []struct {
		name        string
		do          func(*http.Request) (*http.Response, error)
		wantCode    connect.Code
		wantMessage string
	}{
		{name: "passes_response_through", do: pass.Do},
		{name: "timeout", do: withTimeout.Do, wantCode: connect.CodeInternal},
		{
			name: "nil_response",
			do: func(*http.Request) (*http.Response, error) {
				return nil, nil //nolint:nilnil // a broken HTTPClient
			},
			wantCode: connect.CodeInternal,
		},
		{
			name: "nil_body",
			do: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusSwitchingProtocols}, nil
			},
			wantCode: connect.CodeInternal,
		},
		{
			name: "redirect",
			do: func(request *http.Request) (*http.Response, error) {
				if err := request.Context().Err(); err != nil {
					return nil, err
				}
				header := http.Header{"Location": {server.URL() + "/again"}}
				return &http.Response{StatusCode: http.StatusFound, Header: header, Body: http.NoBody}, nil
			},
			wantCode: connect.CodeUnknown,
		},
		{
			// Spec Section 4.5: a Connect error body gives the code and message.
			name: "error_body",
			do: func(*http.Request) (*http.Response, error) {
				body := io.NopCloser(strings.NewReader(`{"code":"unauthenticated","message":"no token"}`))
				return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: body}, nil
			},
			wantCode:    connect.CodeUnauthenticated,
			wantMessage: "no token",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := pingv1connect.NewPingServiceClient(connect.NewClient(
				connecthttp.NewTransport(httpClientFunc(test.do), server.URL(), connecthttp.WithWebSocket()),
			))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 7})
			if test.wantCode == 0 {
				assert.Nil(t, err)
				return
			}
			assert.Equal(t, connect.CodeOf(err), test.wantCode)
			if test.wantMessage != "" {
				var connectErr *connect.Error
				assert.True(t, errors.As(err, &connectErr))
				assert.Equal(t, connectErr.Message(), test.wantMessage)
			}
		})
	}
}

func TestWebSocketClientDisconnect(t *testing.T) {
	t.Parallel()
	// Spec Section 8: a browser that navigates away closes with 1001, which
	// is a cancellation too.
	closes := map[string]func(*websocket.Conn) error{
		"close_now":  (*websocket.Conn).CloseNow,
		"going_away": func(conn *websocket.Conn) error { return conn.Close(websocket.StatusGoingAway, "") },
	}
	for name, closeConn := range closes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			t.Run("handler_running", func(t *testing.T) {
				t.Parallel()
				pingServer := &websocketPingServer{sleeping: make(chan struct{}), sleepCause: make(chan error, 1)}
				server := newWebSocketServer(t, pingServer)
				conn := dialWebSocket(t, server, pingv1connect.PingServicePingProcedure, "connectrpc.1+json", nil)
				assert.Nil(t, writeFrames(t.Context(), conn, textFrame("M[]"), textFrame(`B{"text":"sleep"}`), textFrame("C")))
				<-pingServer.sleeping
				_ = closeConn(conn)
				err := receiveWithin(t, pingServer.sleepCause, "handler cancellation")
				assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
			})
			t.Run("before_end_stream", func(t *testing.T) {
				t.Parallel()
				pingServer := &websocketPingServer{receiveErr: make(chan error, 1)}
				server := newWebSocketServer(t, pingServer)
				conn := dialWebSocket(t, server, pingv1connect.PingServiceSumProcedure, "connectrpc.1+json", nil)
				assert.Nil(t, writeFrames(t.Context(), conn, textFrame("M[]"), textFrame(`B{"number":1}`)))
				_ = closeConn(conn)
				err := receiveWithin(t, pingServer.receiveErr, "Receive error")
				assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
			})
		})
	}
}

func TestWebSocketServerContextEndsWhileHandlerRuns(t *testing.T) {
	t.Parallel()
	// The handler sets metadata after its context ends. Run with -race.
	newServer := func(t *testing.T, handlerDone chan<- struct{}) *memhttp.Server {
		t.Helper()
		return newWebSocketServer(t, &pluggablePingServer{
			cumSum: func(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
				defer close(handlerDone)
				if _, err := stream.Receive(); !errors.Is(err, io.EOF) {
					return err
				}
				<-ctx.Done()
				info, _ := connect.CallInfoForServerContext(ctx)
				info.ResponseHeader().Set("X-Late", "header")
				info.ResponseTrailer().Set("X-Late", "trailer")
				return nil
			},
		})
	}
	t.Run("client_drops", func(t *testing.T) {
		t.Parallel()
		handlerDone := make(chan struct{})
		server := newServer(t, handlerDone)
		conn := dialWebSocket(t, server, pingv1connect.PingServiceCumSumProcedure, "connectrpc.1+json", nil)
		assert.Nil(t, writeFrames(t.Context(), conn, textFrame("M[]"), textFrame("C")))
		_ = conn.CloseNow()
		<-handlerDone
	})
	t.Run("deadline", func(t *testing.T) {
		t.Parallel()
		handlerDone := make(chan struct{})
		server := newServer(t, handlerDone)
		conn := dialWebSocket(t, server, pingv1connect.PingServiceCumSumProcedure+"?connect-timeout-ms=10", "connectrpc.1+json", nil)
		assert.Nil(t, writeFrames(t.Context(), conn, textFrame("M[]"), textFrame("C")))
		frames, closeErr := readFrames(t.Context(), conn)
		<-handlerDone
		assert.Equal(t, closeErr.Code, websocket.StatusNormalClosure)
		assert.Equal(t, frames, []websocketFrame{
			textFrame(`M[["x-late","header"]]`),
			textFrame(`S{"error":{"code":"deadline_exceeded","message":"context deadline exceeded"},"metadata":{"x-late":["trailer"]}}`),
		})
	})
}

func TestWebSocketServerClientDropsWhileSending(t *testing.T) {
	t.Parallel()
	// The handler never calls Receive and the unread B blocks the read loop,
	// yet a failed Send still ends its context.
	clientGone := make(chan struct{})
	canceled := make(chan struct{})
	server := newWebSocketServer(t, &pluggablePingServer{
		cumSum: func(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
			<-clientGone
			var err error
			for err == nil {
				err = stream.Send(&pingv1.CumSumResponse{})
			}
			<-ctx.Done()
			close(canceled)
			return nil
		},
	})
	conn := dialWebSocket(t, server, pingv1connect.PingServiceCumSumProcedure, "connectrpc.1+json", nil)
	assert.Nil(t, writeFrames(t.Context(), conn, textFrame("M[]"), textFrame(`B{"number":1}`)))
	_ = conn.CloseNow()
	close(clientGone)
	receiveWithin(t, canceled, "handler cancellation")
}

func TestWebSocketServerPings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		sends  []time.Duration
		noPong bool
		pings  int32
	}{
		{name: "idle", pings: 3},
		{name: "busy", sends: []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second, 40 * time.Second, 50 * time.Second, 60 * time.Second, 70 * time.Second, 80 * time.Second, 90 * time.Second}},
		{name: "send_then_idle", sends: []time.Duration{20 * time.Second}, pings: 2},
		{name: "unanswered", noPong: true, pings: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				server := newWebSocketServer(t, &pluggablePingServer{
					cumSum: func(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
						start := time.Now()
						for _, at := range test.sends {
							time.Sleep(time.Until(start.Add(at)))
							if err := stream.Send(&pingv1.CumSumResponse{}); err != nil {
								return err
							}
						}
						<-ctx.Done()
						return nil
					},
				})
				var pings atomic.Int32
				conn, response, err := websocket.Dial(t.Context(), server.URL()+pingv1connect.PingServiceCumSumProcedure, &websocket.DialOptions{
					HTTPClient:   &http.Client{Transport: server.TransportHTTP1()},
					Subprotocols: []string{"connectrpc.1+json"},
					OnPingReceived: func(context.Context, []byte) bool {
						pings.Add(1)
						return !test.noPong
					},
				})
				assert.Nil(t, err)
				if response.Body != nil {
					_ = response.Body.Close()
				}
				assert.Nil(t, writeFrames(t.Context(), conn, textFrame("M[]")))
				go func() {
					for {
						if _, _, err := conn.Read(context.Background()); err != nil {
							return
						}
					}
				}()
				time.Sleep(95 * time.Second)
				assert.Equal(t, pings.Load(), test.pings)
				_ = conn.CloseNow()
				// Outlast coder's 5s close handshake before the bubble ends.
				time.Sleep(10 * time.Second)
			})
		})
	}
}

func TestWebSocketDeadlineWithTraffic(t *testing.T) {
	t.Parallel()
	// Arriving messages don't move the deadline, as RFC 008 Section 11.1 requires.
	t.Run("server", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			server := newWebSocketServer(t, &websocketPingServer{})
			start := time.Now()
			procedure := pingv1connect.PingServiceSumProcedure + "?connect-timeout-ms=100"
			conn := dialWebSocket(t, server, procedure, "connectrpc.1+json", nil)
			go func() {
				err := writeFrames(context.Background(), conn, textFrame("M[]"))
				for err == nil && time.Since(start) < 300*time.Millisecond {
					err = writeFrames(context.Background(), conn, textFrame(`B{"number":1}`))
					time.Sleep(7 * time.Millisecond)
				}
			}()
			var frames []websocketFrame
			for len(frames) == 0 || frames[len(frames)-1].Data[0] != 'S' {
				messageType, data, err := conn.Read(t.Context())
				assert.Nil(t, err)
				frames = append(frames, websocketFrame{MessageType: messageType, Data: string(data)})
			}
			assert.Equal(t, time.Since(start), 100*time.Millisecond)
			assert.Equal(t, frames, []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"deadline_exceeded","message":"context deadline exceeded"}}`),
			})
			_, closeErr := readFrames(t.Context(), conn)
			assert.Equal(t, closeErr.Code, websocket.StatusNormalClosure)
			// Outlast coder's 5s close handshake before the bubble ends.
			time.Sleep(10 * time.Second)
		})
	})
	t.Run("client", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			closeCode := make(chan websocket.StatusCode, 1)
			handler := func(response http.ResponseWriter, request *http.Request) {
				options := &websocket.AcceptOptions{Subprotocols: []string{"connectrpc.1+proto"}}
				conn, err := websocket.Accept(response, request, options)
				if err != nil {
					return
				}
				defer func() { _ = conn.CloseNow() }()
				ctx := context.WithoutCancel(request.Context())
				go func() {
					_, closeErr := readFrames(ctx, conn)
					closeCode <- closeErr.Code
				}()
				sendStart := time.Now()
				err = writeFrames(ctx, conn, textFrame("M[]"))
				for err == nil && time.Since(sendStart) < 300*time.Millisecond {
					err = writeFrames(ctx, conn, binaryFrame("B"))
					time.Sleep(7 * time.Millisecond)
				}
			}
			server := memhttptest.NewServer(t, http.HandlerFunc(handler))
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
				&http.Client{Transport: server.TransportHTTP1()}, server.URL(), connecthttp.WithWebSocket(),
			)))
			start := time.Now()
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 1})
			assert.Nil(t, err)
			for err == nil {
				_, err = stream.Receive()
			}
			assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded)
			assert.Equal(t, time.Since(start), 100*time.Millisecond)
			assert.Equal(t, <-closeCode, websocket.StatusNormalClosure)
			// Outlast coder's 5s close handshake before the bubble ends.
			time.Sleep(10 * time.Second)
		})
	})
}

func TestWebSocketServerMetadataLimit(t *testing.T) {
	t.Parallel()
	// M is bounded like HTTP headers, whatever the message read limit.
	tooBig := `M[["x-big","` + strings.Repeat("a", http.DefaultMaxHeaderBytes) + `"]]`
	tests := []struct {
		name    string
		options []connecthttp.Option
	}{
		// The default 4 MiB read limit is already above the header limit.
		{name: "default"},
		{name: "unlimited_messages", options: []connecthttp.Option{connecthttp.WithReadMaxBytes(0)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := newWebSocketServer(t, &pluggablePingServer{
				cumSum: func(context.Context, pingv1connect.PingServiceCumSumServerStream) error {
					t.Error("handler ran")
					return nil
				},
			}, test.options...)
			conn := dialWebSocket(t, server, pingv1connect.PingServiceCumSumProcedure, "connectrpc.1+json", nil)
			go func() { _ = writeFrames(t.Context(), conn, textFrame(tooBig)) }()
			frames, closeErr := readFrames(t.Context(), conn)
			assert.Equal(t, len(frames), 0)
			assert.Equal(t, closeErr.Code, websocket.StatusMessageTooBig)
		})
	}
}

func TestWebSocketServerFragmentedReadLimit(t *testing.T) {
	t.Parallel()
	server := newWebSocketServer(t, &websocketPingServer{}, connecthttp.WithReadMaxBytes(64))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := dialWebSocket(t, server, pingv1connect.PingServicePingProcedure, "connectrpc.1+json", nil)
	go func() {
		if writeFrames(ctx, conn, textFrame("M[]")) != nil {
			return
		}
		// Each Write sends a frame with FIN unset and Close sends the final frame.
		writer, err := conn.Writer(ctx, websocket.MessageText)
		if err != nil {
			return
		}
		for _, fragment := range []string{`B{"number":"7"}`, strings.Repeat(" ", 25), strings.Repeat(" ", 26)} {
			if _, err := io.WriteString(writer, fragment); err != nil {
				return
			}
		}
		_ = writer.Close()
	}()
	got, closeErr := readFrames(ctx, conn)
	assert.Equal(t, len(got), 0)
	assert.Equal(t, closeErr.Code, websocket.StatusMessageTooBig)
}

func TestWebSocketServerReceiveAfterDecodeError(t *testing.T) {
	t.Parallel()
	results := make(chan error, 3)
	server := newWebSocketServer(t, &pluggablePingServer{
		cumSum: func(_ context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
			for range 3 {
				_, err := stream.Receive()
				results <- err
			}
			return nil
		},
	})
	for range 50 {
		conn := dialWebSocket(t, server, pingv1connect.PingServiceCumSumProcedure, "connectrpc.1+json", nil)
		go func() {
			_ = writeFrames(t.Context(), conn, textFrame("M[]"), textFrame(`B{"number":"x"}`), textFrame(`B{"number":"1"}`), textFrame("C"))
		}()
		for range 3 {
			err := receiveWithin(t, results, "Receive result")
			assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument)
		}
		assert.Nil(t, conn.CloseNow())
	}
}

func TestWebSocketServerStalls(t *testing.T) {
	t.Parallel()
	server := newWebSocketServer(t, &websocketPingServer{}, connecthttp.WithReadMaxBytes(1024))
	metadata := append(rawFrame(0x1, 3), "M[]"...)
	tests := []struct {
		name  string
		query string
		send  []byte
		delay time.Duration
	}{
		// The message is over the read limit.
		{name: "oversized_message", send: slices.Concat(metadata, rawFrame(0x2, 1<<20), make([]byte, 4096))},
		// Q fails the RPC while the next message is incomplete.
		{name: "failed", send: slices.Concat(metadata, rawFrame(0x1, 1), []byte("Q"), rawFrame(0x2, 1<<20), make([]byte, 16))},
		// The deadline passes mid-message and then more bytes arrive.
		{name: "deadline", query: "?connect-timeout-ms=200", send: slices.Concat(metadata, rawFrame(0x2, 1<<20), make([]byte, 512)), delay: 600 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			conn, reader := dialWebSocketRaw(t, server, pingv1connect.PingServiceCumSumProcedure+test.query)
			// Read first, or the server's writes block on the in-memory connection.
			closed := make(chan error, 1)
			go func() {
				_, err := io.Copy(io.Discard, reader)
				closed <- err
			}()
			// The server may close mid-write.
			_, _ = conn.Write(test.send)
			if test.delay > 0 {
				time.Sleep(test.delay)
				_, _ = conn.Write(make([]byte, 4096))
			}
			select {
			case <-closed:
			case <-time.After(15 * time.Second): // coder's close handshake waits 5s
				t.Fatal("server didn't close the connection")
			}
		})
	}
}

func TestWebSocketClientStalls(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		cancel   bool
		wantCode connect.Code
	}{
		{name: "oversized_message", wantCode: connect.CodeResourceExhausted},
		{name: "cancel", cancel: true, wantCode: connect.CodeCanceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			closed := make(chan error, 1)
			sent := make(chan struct{})
			server := rawWebSocketServer(t, func(conn net.Conn, reader *bufio.Reader) {
				go func() {
					_, _ = conn.Write(append(rawServerFrame(0x1, 3), "M[]"...))
					_, _ = conn.Write(append(rawServerFrame(0x2, 1<<20), make([]byte, 512)...))
					close(sent)
					if test.cancel {
						time.Sleep(100 * time.Millisecond)
					}
					_, _ = conn.Write(make([]byte, 4096))
				}()
				_, err := io.Copy(io.Discard, reader)
				closed <- err
			})
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
				&http.Client{Transport: server.TransportHTTP1()}, server.URL(),
				connecthttp.WithWebSocket(), connecthttp.WithReadMaxBytes(1024),
			)))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stream, err := client.CumSum(ctx)
			assert.Nil(t, err)
			assert.Nil(t, stream.SendHeaders())
			received := make(chan error, 1)
			go func() {
				_, err := stream.Receive()
				received <- err
			}()
			if test.cancel {
				<-sent
				cancel()
			}
			assert.Equal(t, connect.CodeOf(receiveWithin(t, received, "Receive result")), test.wantCode)
			if test.cancel {
				return
			}
			_ = receiveWithin(t, closed, "client close")
		})
	}
}

func TestWebSocketClientDialDropped(t *testing.T) {
	t.Parallel()
	server := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		if conn, _, err := http.NewResponseController(response).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
		&http.Client{Transport: server.TransportHTTP1()}, server.URL(), connecthttp.WithWebSocket(),
	)))
	_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable)
	assert.False(t, errors.Is(err, io.EOF))
}

func TestWebSocketClientDialError(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	notEnabled := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(notEnabled, &websocketPingServer{})
	connecthttp.Mount(mux, notEnabled)
	server := memhttptest.NewServer(t, mux)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
		&http.Client{Transport: server.TransportHTTP1()}, server.URL(), connecthttp.WithWebSocket(),
	)))
	assertBadStatus := func(t *testing.T, err error) {
		t.Helper()
		assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
		assert.True(t, strings.Contains(err.Error(), "HTTP status 400"), assert.Sprintf("error %q", err))
	}
	t.Run("close_send", func(t *testing.T) {
		t.Parallel()
		stream, err := client.CumSum(t.Context())
		assert.Nil(t, err)
		assertBadStatus(t, stream.CloseSend())
	})
	t.Run("receive", func(t *testing.T) {
		t.Parallel()
		stream, err := client.CumSum(t.Context())
		assert.Nil(t, err)
		_, err = stream.Receive()
		assertBadStatus(t, err)
		_, again := stream.Receive()
		assert.ErrorIs(t, again, err)
	})
	t.Run("canceled", func(t *testing.T) {
		t.Parallel()
		started := make(chan struct{})
		hangingServer := memhttptest.NewServer(t, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			close(started)
			<-request.Context().Done()
		}))
		hangingClient := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
			&http.Client{Transport: hangingServer.TransportHTTP1()}, hangingServer.URL(), connecthttp.WithWebSocket(),
		)))
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			<-started
			cancel()
		}()
		_, err := hangingClient.Ping(ctx, &pingv1.PingRequest{})
		assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
	})
}

func TestWebSocketServerFirstMessageTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		server := newWebSocketServer(t, &websocketPingServer{})
		conn := dialWebSocket(t, server, pingv1connect.PingServiceCumSumProcedure, "connectrpc.1+json", nil)
		start := time.Now()
		frames, closeErr := readFrames(context.Background(), conn)
		assert.Equal(t, time.Since(start), 10*time.Second)
		assert.Equal(t, frames, []websocketFrame{
			textFrame("M[]"),
			textFrame(`S{"error":{"code":"deadline_exceeded","message":"no M message within 10s"}}`),
		})
		assert.Equal(t, closeErr.Code, websocket.StatusNormalClosure)
		_ = conn.CloseNow()
		// Outlast coder's 5s close handshake before the bubble ends.
		time.Sleep(10 * time.Second)
	})
}

func TestWebSocketClientMarshalError(t *testing.T) {
	t.Parallel()
	server := newWebSocketServer(t, &websocketPingServer{})
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
		&http.Client{Transport: server.TransportHTTP1()}, server.URL(), connecthttp.WithWebSocket(),
	)))
	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Text: "\xff"})
	assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
	assert.True(t, strings.Contains(err.Error(), "marshal message"), assert.Sprintf("error %q", err))
}

func TestWebSocketClientInvalidMetadata(t *testing.T) {
	t.Parallel()
	var handshakes atomic.Int32
	mux := http.NewServeMux()
	connectServer := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(connectServer, &websocketPingServer{})
	connecthttp.Mount(mux, connectServer, connecthttp.WithWebSocket())
	server := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		handshakes.Add(1)
		mux.ServeHTTP(response, request)
	}))
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
		&http.Client{Transport: server.TransportHTTP1()}, server.URL(), connecthttp.WithWebSocket(),
	)))
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "reserved_key", key: "Cookie", value: "a"},
		{name: "invalid_key", key: "Bad Key", value: "a"},
		{name: "invalid_value", key: "X-Test", value: "a\nb"},
	}
	// Cleanup runs after the parallel subtests.
	t.Cleanup(func() { assert.Equal(t, handshakes.Load(), 0) })
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, info := connect.NewClientContext(t.Context())
			info.RequestHeader().Set(test.key, test.value)
			_, err := client.Ping(ctx, &pingv1.PingRequest{})
			assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
		})
	}
}

func TestWebSocketClientSendAfterEnd(t *testing.T) {
	t.Parallel()
	server := newWebSocketServer(t, &websocketPingServer{})
	serverClosed := make(chan struct{})
	closingServer := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		options := &websocket.AcceptOptions{Subprotocols: []string{"connectrpc.1+proto"}}
		conn, err := websocket.Accept(response, request, options)
		if err != nil {
			return
		}
		_, _, _ = conn.Read(request.Context())
		_ = conn.CloseNow()
		close(serverClosed)
	}))
	tests := []struct {
		name   string
		server *memhttp.Server
		end    func(pingv1connect.PingServiceCumSumClientStream) error
	}{
		{
			name:   "close_send",
			server: server,
			end: func(stream pingv1connect.PingServiceCumSumClientStream) error {
				return stream.CloseSend()
			},
		},
		{
			name:   "close",
			server: server,
			end: func(stream pingv1connect.PingServiceCumSumClientStream) error {
				return stream.Close()
			},
		},
		{
			name:   "write_failure",
			server: closingServer,
			end: func(stream pingv1connect.PingServiceCumSumClientStream) error {
				err := stream.SendHeaders()
				<-serverClosed
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
				&http.Client{Transport: test.server.TransportHTTP1()}, test.server.URL(), connecthttp.WithWebSocket(),
			)))
			stream, err := client.CumSum(t.Context())
			assert.Nil(t, err)
			assert.Nil(t, test.end(stream))
			assert.ErrorIs(t, stream.Send(&pingv1.CumSumRequest{Number: 1}), io.EOF)
		})
	}
}

func TestWebSocketClientTrailerReplaces(t *testing.T) {
	t.Parallel()
	server := newWebSocketServer(t, &websocketPingServer{})
	prefill := func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				info.ResponseTrailer().Set("X-Test", "stale")
			}
			return next(ctx, spec)
		}
	}
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
		&http.Client{Transport: server.TransportHTTP1()}, server.URL(), connecthttp.WithWebSocket(),
	), prefill))
	ctx, info := connect.NewClientContext(t.Context())
	info.RequestHeader().Set("X-Test", "fresh")
	_, err := client.Ping(ctx, &pingv1.PingRequest{})
	assert.Nil(t, err)
	assert.Equal(t, info.ResponseTrailer().Values("X-Test"), []string{"fresh"})
}

// websocketPingServer adds to pingServer the server call-info check, a
// handler that sleeps until canceled, invalid or reserved response metadata,
// and a Sum receive-error hook.
type websocketPingServer struct {
	pingServer

	sleeping   chan struct{}
	sleepCause chan error
	receiveErr chan error
}

func (p *websocketPingServer) Ping(ctx context.Context, request *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	if info.Protocol != "connect-websocket" || info.Spec.Procedure != pingv1connect.PingServicePingProcedure {
		return nil, connect.Errorf(connect.CodeInternal, "unexpected call info %q %q", info.Protocol, info.Spec.Procedure)
	}
	if serverInfo, ok := connecthttp.ServerInfoForContext(ctx); !ok || serverInfo.HTTPMethod() != http.MethodGet {
		return nil, connect.NewError(connect.CodeInternal, "missing server info")
	}
	switch request.GetText() {
	case "sleep":
		if p.sleeping != nil {
			close(p.sleeping)
		}
		<-ctx.Done()
		if p.sleepCause != nil {
			p.sleepCause <- context.Cause(ctx)
		}
		return nil, ctx.Err()
	case "connect-header":
		info.ResponseHeader().Set("X-Ok", "1")
		info.ResponseHeader().Set("Connect-Custom", "1")
	case "connect-trailer":
		info.ResponseTrailer().Set("X-Ok", "1")
		info.ResponseTrailer().Set("Connect-Custom", "1")
	case "empty-metadata":
		info.ResponseHeader().SetValues("Connect-Empty", nil)
		info.ResponseTrailer().SetValues("Connect-Empty", nil)
	case "bad-metadata":
		for _, header := range []*connect.Header{info.ResponseHeader(), info.ResponseTrailer()} {
			header.Add("X-Ok", "1")
			header.Add("X-Ok", "bad\nvalue")
			header.Add("X-Bad", "bad\nvalue")
			header.Add("Bad Key", "1")
		}
	}
	if value := info.RequestHeader().Get("X-Test"); value != "" {
		info.ResponseHeader().Set("X-Test", value)
		info.ResponseTrailer().Set("X-Test", value)
	}
	return &pingv1.PingResponse{Number: request.GetNumber(), Text: request.GetText()}, nil
}

func (p *websocketPingServer) Sum(_ context.Context, stream pingv1connect.PingServiceSumServerStream) (*pingv1.SumResponse, error) {
	var sum int64
	for {
		request, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return &pingv1.SumResponse{Sum: sum}, nil
		} else if err != nil {
			if p.receiveErr != nil {
				p.receiveErr <- err
			}
			return nil, err
		}
		sum += request.GetNumber()
	}
}

type websocketFrame struct {
	MessageType websocket.MessageType
	Data        string
}

func textFrame(data string) websocketFrame {
	return websocketFrame{MessageType: websocket.MessageText, Data: data}
}

func binaryFrame(data string) websocketFrame {
	return websocketFrame{MessageType: websocket.MessageBinary, Data: data}
}

// writeFrames stops at the first error, which is usually the peer closing.
func writeFrames(ctx context.Context, conn *websocket.Conn, frames ...websocketFrame) error {
	for _, frame := range frames {
		if err := conn.Write(ctx, frame.MessageType, []byte(frame.Data)); err != nil {
			return err
		}
	}
	return nil
}

// readFrames reads until the connection ends. The CloseError has Code -1
// when no Close frame arrived.
func readFrames(ctx context.Context, conn *websocket.Conn) ([]websocketFrame, websocket.CloseError) {
	var frames []websocketFrame
	for {
		messageType, data, err := conn.Read(ctx)
		if err != nil {
			closeErr := websocket.CloseError{Code: -1}
			_ = errors.As(err, &closeErr)
			return frames, closeErr
		}
		frames = append(frames, websocketFrame{MessageType: messageType, Data: string(data)})
	}
}

func receiveWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	var value T
	select {
	case value = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s within 5s", what)
	}
	return value
}

func newWebSocketServer(t *testing.T, handler pingv1connect.PingServiceHandler, options ...connecthttp.Option) *memhttp.Server {
	t.Helper()
	return newWebSocketServerWith(t, connect.NewServer(), handler, options...)
}

func newWebSocketServerWith(
	t *testing.T,
	server *connect.Server,
	handler pingv1connect.PingServiceHandler,
	options ...connecthttp.Option,
) *memhttp.Server {
	t.Helper()
	mux := http.NewServeMux()
	pingv1connect.RegisterPingServiceHandler(server, handler)
	connecthttp.Mount(mux, server, append(options, connecthttp.WithWebSocket())...)
	return memhttptest.NewServer(t, mux)
}

func dialWebSocket(t *testing.T, server *memhttp.Server, path, subprotocol string, header http.Header) *websocket.Conn {
	t.Helper()
	conn, response, err := websocket.Dial(t.Context(), server.URL()+path, &websocket.DialOptions{
		HTTPClient:   &http.Client{Transport: server.TransportHTTP1()},
		HTTPHeader:   header,
		Subprotocols: []string{subprotocol},
	})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func dialWebSocketRaw(t *testing.T, server *memhttp.Server, procedure string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := server.TransportHTTP1().DialContext(t.Context(), "tcp", "example.com:80")
	assert.Nil(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = io.WriteString(conn, "GET "+procedure+" HTTP/1.1\r\nHost: example.com\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Protocol: connectrpc.1+proto\r\n\r\n")
	assert.Nil(t, err)
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	assert.Nil(t, err)
	assert.Equal(t, response.StatusCode, http.StatusSwitchingProtocols)
	assert.Nil(t, response.Body.Close())
	return conn, reader
}

func rawWebSocketServer(t *testing.T, serve func(net.Conn, *bufio.Reader)) *memhttp.Server {
	t.Helper()
	return memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, readWriter, err := http.NewResponseController(response).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		sum := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, err = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(sum[:])+"\r\nSec-WebSocket-Protocol: connectrpc.1+proto\r\n\r\n")
		if err != nil {
			return
		}
		serve(conn, readWriter.Reader)
	}))
}

func rawServerFrame(opcode byte, length uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte{0x80 | opcode, 127}, length)
}

// rawFrame is a final client frame header with a zero mask key, which leaves
// the payload as is.
func rawFrame(opcode byte, length uint64) []byte {
	header := binary.BigEndian.AppendUint64([]byte{0x80 | opcode, 0x80 | 127}, length)
	return append(header, 0, 0, 0, 0)
}
