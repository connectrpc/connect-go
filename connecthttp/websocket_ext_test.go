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
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
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
)

func TestWebSocketRoundTrip(t *testing.T) {
	t.Parallel()
	server := newWebSocketServer(t, &websocketPingServer{})
	// memhttp pipes have no buffer, so clients must send and receive
	// concurrently.
	httpClient := &http.Client{Transport: server.TransportHTTP1()}
	for _, codec := range []string{connect.CodecNameProto, connect.CodecNameJSON} {
		t.Run(codec, func(t *testing.T) {
			t.Parallel()
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
				httpClient, server.URL(), connecthttp.WithWebSocket(), connecthttp.WithSendCodec(codec),
			)))
			t.Run("unary", func(t *testing.T) {
				t.Parallel()
				ctx, info := connect.NewClientContext(t.Context())
				info.RequestHeader().Set("X-Test", "value")
				response, err := client.Ping(ctx, &pingv1.PingRequest{Number: 7, Text: "hello"})
				assert.Nil(t, err)
				assert.Equal(t, response.GetNumber(), 7)
				assert.Equal(t, response.GetText(), "hello")
				assert.Equal(t, info.Protocol, "connect-websocket")
				assert.Equal(t, info.Codec, codec)
				assert.Equal(t, info.ResponseHeader().Get("X-Test"), "value")
				assert.Equal(t, info.ResponseTrailer().Get("X-Test"), "value")
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
			t.Run("scrubbed_error", func(t *testing.T) {
				t.Parallel()
				_, err := client.Ping(t.Context(), &pingv1.PingRequest{Text: "secret"})
				var connectErr *connect.Error
				assert.True(t, errors.As(err, &connectErr))
				assert.Equal(t, connectErr.Code(), connect.CodeUnknown)
				assert.Equal(t, connectErr.Message(), "")
			})
			t.Run("deadline", func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				_, err := client.Ping(ctx, &pingv1.PingRequest{Text: "sleep"})
				assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded)
			})
			t.Run("client_stream", func(t *testing.T) {
				t.Parallel()
				stream, err := client.Sum(t.Context())
				assert.Nil(t, err)
				for i := range 3 {
					assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: int64(i + 1)}))
				}
				response, err := stream.CloseAndReceive()
				assert.Nil(t, err)
				assert.Equal(t, response.GetSum(), 6)
			})
			t.Run("server_stream", func(t *testing.T) {
				t.Parallel()
				stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 3})
				assert.Nil(t, err)
				var got []int64
				for {
					response, err := stream.Receive()
					if errors.Is(err, io.EOF) || !assert.Nil(t, err) {
						break
					}
					got = append(got, response.GetNumber())
				}
				assert.Equal(t, got, []int64{1, 2, 3})
			})
			t.Run("send_max_bytes", func(t *testing.T) {
				t.Parallel()
				client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
					httpClient, server.URL(), connecthttp.WithWebSocket(), connecthttp.WithSendCodec(codec), connecthttp.WithSendMaxBytes(4),
				)))
				_, err := client.Ping(t.Context(), &pingv1.PingRequest{Text: "too long"})
				assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
			})
			t.Run("close_while_receiving", func(t *testing.T) {
				t.Parallel()
				stream, err := client.CumSum(t.Context())
				assert.Nil(t, err)
				assert.Nil(t, stream.SendHeaders())
				time.AfterFunc(10*time.Millisecond, func() { _ = stream.Close() })
				_, err = stream.Receive()
				assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
			})
			t.Run("bidi", func(t *testing.T) {
				t.Parallel()
				stream, err := client.CumSum(t.Context())
				assert.Nil(t, err)
				sendErr := make(chan error, 1)
				go func() {
					for i := range 3 {
						if err := stream.Send(&pingv1.CumSumRequest{Number: int64(i + 1)}); err != nil {
							sendErr <- err
							return
						}
					}
					sendErr <- stream.CloseSend()
				}()
				var got []int64
				for {
					response, err := stream.Receive()
					if errors.Is(err, io.EOF) || !assert.Nil(t, err) {
						break
					}
					got = append(got, response.GetSum())
				}
				assert.Nil(t, <-sendErr)
				assert.Equal(t, got, []int64{1, 3, 6})
			})
		})
	}
}

func TestWebSocketWire(t *testing.T) {
	t.Parallel()
	server := newWebSocketServer(t, &websocketPingServer{})
	limitedServer := newWebSocketServer(t, &websocketPingServer{}, connecthttp.WithSendMaxBytes(8))
	pingRequest := `{"number":"7"}`
	tests := []struct {
		name        string
		server      *memhttp.Server
		subprotocol string
		query       string
		send        []websocketFrame
		want        []websocketFrame
		wantStatus  websocket.StatusCode
	}{
		{
			name:        "json",
			subprotocol: "connectrpc.1+json",
			send:        []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("C")},
			want:        []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("S{}")},
			wantStatus:  websocket.StatusNormalClosure,
		},
		{
			name:        "proto",
			subprotocol: "connectrpc.1+proto",
			send:        []websocketFrame{textFrame("M[]"), binaryFrame("B\x08\x07"), textFrame("C")},
			want:        []websocketFrame{textFrame("M[]"), binaryFrame("B\x08\x07"), textFrame("S{}")},
			wantStatus:  websocket.StatusNormalClosure,
		},
		{
			name:        "invalid_response_metadata",
			subprotocol: "connectrpc.1+json",
			send:        []websocketFrame{textFrame("M[]"), textFrame(`B{"text":"bad-metadata"}`), textFrame("C")},
			want: []websocketFrame{
				textFrame(`M[["x-ok","1"]]`),
				textFrame(`B{"text":"bad-metadata"}`),
				textFrame(`S{"metadata":{"x-ok":["1"]}}`),
			},
			wantStatus: websocket.StatusNormalClosure,
		},
		{
			name:        "send_max_bytes",
			server:      limitedServer,
			subprotocol: "connectrpc.1+json",
			send:        []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"resource_exhausted","message":"message size 14 exceeds sendMaxBytes 8"}}`),
			},
			wantStatus: websocket.StatusNormalClosure,
		},
		{
			name:        "missing_metadata",
			subprotocol: "connectrpc.1+json",
			send:        []websocketFrame{textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"internal","message":"protocol error: first message is B, expected M"}}`),
			},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name:        "reserved_metadata",
			subprotocol: "connectrpc.1+json",
			send:        []websocketFrame{textFrame(`M[["x-forwarded-for","10.0.0.1"]]`), textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"internal","message":"protocol error: M message key \"x-forwarded-for\" is reserved"}}`),
			},
			wantStatus: websocket.StatusProtocolError,
		},
		{
			name:        "deadline_while_receiving",
			subprotocol: "connectrpc.1+json",
			query:       "?connect-timeout-ms=50",
			send:        []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest)},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"deadline_exceeded","message":"context deadline exceeded"}}`),
			},
			wantStatus: websocket.StatusNormalClosure,
		},
		{
			name:        "zero_timeout",
			subprotocol: "connectrpc.1+json",
			query:       "?connect-timeout-ms=0",
			send:        []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"deadline_exceeded","message":"context deadline exceeded"}}`),
			},
			wantStatus: websocket.StatusNormalClosure,
		},
		{
			name:        "malformed_timeout",
			subprotocol: "connectrpc.1+json",
			query:       "?connect-timeout-ms=1%zz",
			send:        []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"invalid_argument","message":"parse timeout: invalid URL escape \"%zz\""}}`),
			},
			wantStatus: websocket.StatusNormalClosure,
		},
		{
			name:        "invalid_timeout",
			subprotocol: "connectrpc.1+json",
			query:       "?connect-timeout-ms=-1",
			send:        []websocketFrame{textFrame("M[]"), textFrame("B" + pingRequest), textFrame("C")},
			want: []websocketFrame{
				textFrame("M[]"),
				textFrame(`S{"error":{"code":"invalid_argument","message":"parse timeout: \"-1\" has non-digit characters"}}`),
			},
			wantStatus: websocket.StatusNormalClosure,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			testServer := server
			if test.server != nil {
				testServer = test.server
			}
			conn := dialWebSocket(t, testServer, pingv1connect.PingServicePingProcedure+test.query, test.subprotocol)
			go func() {
				for _, frame := range test.send {
					if err := conn.Write(ctx, frame.MessageType, []byte(frame.Data)); err != nil {
						return
					}
				}
			}()
			var got []websocketFrame
			for {
				messageType, data, err := conn.Read(ctx)
				if err != nil {
					assert.Equal(t, websocket.CloseStatus(err), test.wantStatus)
					break
				}
				got = append(got, websocketFrame{MessageType: messageType, Data: string(data)})
			}
			assert.Equal(t, got, test.want)
		})
	}
	t.Run("unsupported_subprotocol", func(t *testing.T) {
		t.Parallel()
		_, response, err := websocket.Dial(t.Context(), server.URL()+pingv1connect.PingServicePingProcedure, &websocket.DialOptions{
			HTTPClient:   &http.Client{Transport: server.TransportHTTP1()},
			Subprotocols: []string{"connectrpc.1+xml"},
		})
		assert.NotNil(t, err)
		assert.Equal(t, response.StatusCode, http.StatusBadRequest)
		assert.Nil(t, response.Body.Close())
	})
}

func TestWebSocketClientProtocolErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		noSubprotocol bool
		send          []websocketFrame
		wantStatus    websocket.StatusCode
	}{
		{name: "no_subprotocol", noSubprotocol: true, wantStatus: 3102},
		{name: "missing_metadata", send: []websocketFrame{binaryFrame("B")}, wantStatus: 3102},
		{name: "second_metadata", send: []websocketFrame{textFrame("M[]"), textFrame("M[]")}, wantStatus: 3102},
		{name: "client_end_stream", send: []websocketFrame{textFrame("M[]"), textFrame("C")}, wantStatus: 3102},
		{name: "null_end_stream", send: []websocketFrame{textFrame("M[]"), textFrame("Snull")}, wantStatus: 3102},
		{name: "invalid_utf8", send: []websocketFrame{textFrame("M\xff")}, wantStatus: websocket.StatusInvalidFramePayloadData},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			closeStatus := make(chan websocket.StatusCode, 1)
			server := memhttptest.NewServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				options := &websocket.AcceptOptions{Subprotocols: []string{"connectrpc.1+proto"}}
				if test.noSubprotocol {
					options = nil
				}
				conn, err := websocket.Accept(response, request, options)
				if err != nil {
					return
				}
				// net/http cancels the request context when the handler returns.
				ctx := context.WithoutCancel(request.Context())
				go func(ctx context.Context) {
					for {
						if _, _, err := conn.Read(ctx); err != nil {
							closeStatus <- websocket.CloseStatus(err)
							return
						}
					}
				}(ctx)
				for _, frame := range test.send {
					_ = conn.Write(ctx, frame.MessageType, []byte(frame.Data))
				}
			}))
			client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(
				&http.Client{Transport: server.TransportHTTP1()}, server.URL(), connecthttp.WithWebSocket(),
			)))
			_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
			assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
			select {
			case status := <-closeStatus:
				assert.Equal(t, status, test.wantStatus)
			case <-time.After(5 * time.Second):
				t.Fatal("client did not close")
			}
		})
	}
}

func TestWebSocketClientSilentServer(t *testing.T) {
	t.Parallel()
	// silentServer never answers the client's Close frame.
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
			for _, frame := range send {
				_ = conn.Write(ctx, frame.MessageType, []byte(frame.Data))
			}
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
		// Returns at S, not after coder's 5s close handshake.
		client := newClient(t, silentServer(t, 3, textFrame("M[]"), binaryFrame("B"), textFrame("S{}")))
		start := time.Now()
		_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
		assert.Nil(t, err)
		assert.True(t, time.Since(start) < time.Second)
	})
	t.Run("close_while_receiving", func(t *testing.T) {
		t.Parallel()
		// Close unblocks Receive after the grace period, not coder's 5s.
		client := newClient(t, silentServer(t, 1))
		stream, err := client.CumSum(t.Context())
		assert.Nil(t, err)
		assert.Nil(t, stream.SendHeaders())
		time.AfterFunc(10*time.Millisecond, func() { _ = stream.Close() })
		start := time.Now()
		_, err = stream.Receive()
		assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
		assert.True(t, time.Since(start) < 3*time.Second)
	})
}

func TestWebSocketClientDisconnect(t *testing.T) {
	t.Parallel()
	handlerDone := make(chan error, 1)
	server := newWebSocketServer(t, &websocketPingServer{cumSumDone: handlerDone})
	conn := dialWebSocket(t, server, pingv1connect.PingServiceCumSumProcedure, "connectrpc.1+json")
	assert.Nil(t, conn.Write(t.Context(), websocket.MessageText, []byte("M[]")))
	assert.Nil(t, conn.Write(t.Context(), websocket.MessageText, []byte(`B{"number":"1"}`)))
	for _, want := range []string{"M[]", `B{"sum":"1"}`} {
		_, data, err := conn.Read(t.Context())
		assert.Nil(t, err)
		assert.Equal(t, string(data), want)
	}
	assert.Nil(t, conn.CloseNow())
	select {
	case err := <-handlerDone:
		assert.Equal(t, connect.CodeOf(err), connect.CodeCanceled)
	case <-time.After(5 * time.Second):
		t.Fatal("handler was not canceled")
	}
}

// websocketPingServer is a ping service for WebSocket tests. The shared test
// server echoes Connect-* headers, which WebSocket metadata can't carry.
type websocketPingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	cumSumDone chan error
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
	case "secret":
		return nil, errors.New("internal detail")
	case "sleep":
		<-ctx.Done()
		return nil, ctx.Err()
	case "bad-metadata":
		for _, header := range []*connect.Header{info.ResponseHeader(), info.ResponseTrailer()} {
			header.Add("X-Ok", "1")
			header.Add("X-Ok", "bad\nvalue")
			header.Add("X-Bad", "bad\nvalue")
		}
	}
	if value := info.RequestHeader().Get("X-Test"); value != "" {
		info.ResponseHeader().Set("X-Test", value)
		info.ResponseTrailer().Set("X-Test", value)
	}
	return &pingv1.PingResponse{Number: request.GetNumber(), Text: request.GetText()}, nil
}

func (p *websocketPingServer) Fail(_ context.Context, request *pingv1.FailRequest) (*pingv1.FailResponse, error) {
	detail, err := connectproto.NewErrorDetail(request)
	if err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.Code(request.GetCode()), "oh no").WithDetail(detail)
}

func (p *websocketPingServer) Sum(_ context.Context, stream pingv1connect.PingServiceSumServerStream) (*pingv1.SumResponse, error) {
	var sum int64
	for {
		request, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return &pingv1.SumResponse{Sum: sum}, nil
		} else if err != nil {
			return nil, err
		}
		sum += request.GetNumber()
	}
}

func (p *websocketPingServer) CountUp(_ context.Context, request *pingv1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
	for i := range request.GetNumber() {
		if err := stream.Send(&pingv1.CountUpResponse{Number: i + 1}); err != nil {
			return err
		}
	}
	return nil
}

func (p *websocketPingServer) CumSum(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	err := p.cumSum(stream)
	if p.cumSumDone != nil {
		<-ctx.Done()
		p.cumSumDone <- context.Cause(ctx)
	}
	return err
}

func (p *websocketPingServer) cumSum(stream pingv1connect.PingServiceCumSumServerStream) error {
	var sum int64
	for {
		request, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		sum += request.GetNumber()
		if err := stream.Send(&pingv1.CumSumResponse{Sum: sum}); err != nil {
			return err
		}
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

func newWebSocketServer(t *testing.T, handler pingv1connect.PingServiceHandler, options ...connecthttp.Option) *memhttp.Server {
	t.Helper()
	mux := http.NewServeMux()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, handler)
	connecthttp.Mount(mux, server, append(options, connecthttp.WithWebSocket())...)
	return memhttptest.NewServer(t, mux)
}

func dialWebSocket(t *testing.T, server *memhttp.Server, path, subprotocol string) *websocket.Conn {
	t.Helper()
	conn, response, err := websocket.Dial(t.Context(), server.URL()+path, &websocket.DialOptions{
		HTTPClient:   &http.Client{Transport: server.TransportHTTP1()},
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
