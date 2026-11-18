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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The subprotocol token is the only thing that says which codec is in use, so
// what it selects has to be checked against the bytes on the wire rather than
// against a round trip. A round trip succeeds whenever both ends agree, even
// if they agree on something other than what the token names.
func TestCodecSelectsWireFormat(t *testing.T) {
	t.Parallel()
	const value = int64(7)
	for _, test := range []struct {
		name  string
		token string
		json  bool
	}{
		{name: "proto", token: "connectrpc.1+proto"},
		{name: "json", token: "connectrpc.1+json", json: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newServerFor(t, scriptedServer{})
			client := dialBrowserClientWithToken(
				t, httpServer, pingv1connect.PingServiceSumProcedure, test.token)

			request := &pingv1.SumRequest{Number: value}
			var payload []byte
			var err error
			if test.json {
				payload, err = protojson.Marshal(request)
			} else {
				payload, err = proto.Marshal(request)
			}
			assert.Nil(t, err)
			// The frame type *is* the encoding: JSON goes out as text,
			// Protobuf as binary.
			client.writeMessage(t, wireBody, test.json, payload)
			client.writeJSON(t, wireClientEndStream, nil)

			opening, _, _ := client.readMessage(t)
			assert.Equal(t, opening, wireMetadata)

			marker, body, text := client.readMessage(t)
			assert.Equal(t, marker, wireBody)
			assert.Equal(t, text, test.json)
			if test.json {
				assertProtoJSON(t, body, value)
			} else {
				assertProtoBinary(t, body, value)
			}

			// The terminal message is JSON whichever codec carries the bodies:
			// it is protocol, not payload. A client built for the proto
			// subprotocol still has to parse JSON here, in a text frame.
			marker, body, text = client.readMessage(t)
			assert.Equal(t, marker, wireServerEndStream)
			assert.True(t, text)
			assert.True(t, json.Valid(body))
			assert.True(t, strings.Contains(string(body), "set-by-handler"))
		})
	}
}

// assertProtoBinary checks that body is Protobuf binary carrying sum, and in
// particular that it is not JSON that happens to decode.
func assertProtoBinary(tb testing.TB, body []byte, sum int64) {
	tb.Helper()
	assert.True(tb, !json.Valid(body))
	var response pingv1.SumResponse
	assert.Nil(tb, proto.Unmarshal(body, &response))
	assert.Equal(tb, response.Sum, sum)
}

// assertProtoJSON checks that body is protobuf-JSON carrying sum. The int64 is
// asserted to be a *string*: protobuf-JSON quotes 64-bit integers, so a client
// that parses this as a JSON number loses precision above 2^53. The bytes are
// decoded rather than compared, because protojson varies its whitespace.
func assertProtoJSON(tb testing.TB, body []byte, sum int64) {
	tb.Helper()
	assert.True(tb, json.Valid(body))

	var fields map[string]any
	assert.Nil(tb, json.Unmarshal(body, &fields))
	raw, ok := fields["sum"]
	assert.True(tb, ok)
	quoted, isString := raw.(string)
	assert.True(tb, isString)
	assert.Equal(tb, quoted, strconv.FormatInt(sum, 10))

	var response pingv1.SumResponse
	assert.Nil(tb, protojson.Unmarshal(body, &response))
	assert.Equal(tb, response.Sum, sum)
}

// §6.1: C is a control message like M and S, so it is a bare text frame
// whatever the codec. Only bodies follow the negotiated frame type.
func TestEndOfClientStreamIsAlwaysText(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		codec string
	}{
		{codec: connect.CodecNameProto},
		{codec: connect.CodecNameJSON},
	} {
		t.Run(test.codec, func(t *testing.T) {
			t.Parallel()
			frames := make(chan bool, 4)
			httpServer := frameTypeRecordingServer(t, frames)

			transport := connecthttp.NewTransport(
				httpServer.Client(),
				httpServer.URL,
				connecthttp.WithWebSocket(connecthttp.SelectAll),
				connecthttp.WithSendCodec(test.codec),
			)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
			stream, err := client.Sum(t.Context())
			assert.Nil(t, err)
			// This server never answers, so the call would block; the frames it
			// has already sent are what the test reads.
			go func() { _, _ = stream.CloseAndReceive() }()

			assert.Equal(t, readFrameType(t, frames), true) // opening M is JSON
			assert.Equal(t, readFrameType(t, frames), true) // bare C is text
		})
	}
}

// closeCodeRecordingServer accepts an upgrade by hand and reports the close
// code and reason the client eventually sends, which is the only way to see
// them. A read that ended without a close frame reports code -1.
func closeCodeRecordingServer(tb testing.TB, codes chan<- websocket.CloseError) *httptest.Server {
	tb.Helper()
	httpServer := httptest.NewServer(http.HandlerFunc(
		func(responseWriter http.ResponseWriter, request *http.Request) {
			conn, err := websocket.Accept(responseWriter, request, &websocket.AcceptOptions{
				Subprotocols: []string{firstOfferedSubprotocol(request)},
			})
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			for {
				if _, _, readErr := conn.Read(request.Context()); readErr != nil {
					select {
					case codes <- closeErrorOf(readErr):
					default:
					}
					return
				}
			}
		},
	))
	tb.Cleanup(httpServer.Close)
	return httpServer
}

func closeErrorOf(err error) websocket.CloseError {
	if closeErr, ok := errors.AsType[websocket.CloseError](err); ok {
		return closeErr
	}
	return websocket.CloseError{Code: -1}
}

// §13.1: a client that ends an RPC on its own unexpected condition says so with
// 3102 and a reason, rather than the 1000 that would tell the server the stream
// finished normally. The server behaved here — only this end failed.
func TestClientClosesWithInternalErrorCode(t *testing.T) {
	t.Parallel()
	codes := make(chan websocket.CloseError, 1)
	httpServer := closeCodeRecordingServer(t, codes)

	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
		// Small enough that the message below cannot be encoded for the wire.
		connecthttp.WithSendMaxBytes(16),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	// Far past the limit, so the encode step refuses it before the wire.
	_, callErr := client.Ping(t.Context(), &pingv1.PingRequest{
		Text: strings.Repeat("x", 4096),
	})
	assert.NotNil(t, callErr)
	assert.Equal(t, connect.CodeOf(callErr), connect.CodeResourceExhausted)

	select {
	case closeErr := <-codes:
		assert.Equal(t, closeErr.Code, websocket.StatusCode(3102))
		assert.True(t, strings.Contains(closeErr.Reason, "16"))
		assert.True(t, len(closeErr.Reason) <= 123)
	case <-time.After(10 * time.Second):
		t.Fatal("the client never closed")
	}
}

// A stream that ends with nothing wrong closes 1000, so the code above cannot
// be what this client always sends.
func TestCleanStreamClosesNormally(t *testing.T) {
	t.Parallel()
	codes := make(chan websocket.CloseError, 1)
	httpServer := closeCodeRecordingServer(t, codes)

	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	assert.Nil(t, stream.Close())

	select {
	case closeErr := <-codes:
		assert.Equal(t, closeErr.Code, websocket.StatusNormalClosure)
	case <-time.After(10 * time.Second):
		t.Fatal("the client never closed")
	}
}

// readFrameType takes the next observed frame type, failing rather than
// blocking if the client never sent one.
func readFrameType(tb testing.TB, frames <-chan bool) bool {
	tb.Helper()
	select {
	case text := <-frames:
		return text
	case <-time.After(10 * time.Second):
		tb.Fatal("the client sent no further message")
		return false
	}
}

// frameTypeRecordingServer accepts an upgrade by hand and reports whether each
// client message arrived as text, which is the only way to see a frame type
// from outside.
func frameTypeRecordingServer(tb testing.TB, frames chan<- bool) *httptest.Server {
	tb.Helper()
	httpServer := httptest.NewServer(http.HandlerFunc(
		func(responseWriter http.ResponseWriter, request *http.Request) {
			conn, err := websocket.Accept(responseWriter, request, &websocket.AcceptOptions{
				Subprotocols: []string{firstOfferedSubprotocol(request)},
			})
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			for {
				messageType, _, readErr := conn.Read(request.Context())
				if readErr != nil {
					return
				}
				select {
				case frames <- messageType == websocket.MessageText:
				default:
				}
			}
		},
	))
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// A server configured with one codec is a supported configuration, not a
// broken one. It must negotiate what it has and refuse what it does not,
// rather than upgrading into a connection it cannot decode.
func TestPartialCodecSetNegotiates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		codecs []connect.Codec
		offer  string
		status int
		echoed string
	}{
		{
			name:   "server takes the client's second choice",
			codecs: []connect.Codec{connectproto.NewBinaryCodec()},
			// A codec the server lacks first: a server that failed on the
			// first recognized token would refuse a client it can serve.
			offer:  "connectrpc.1+msgpack, connectrpc.1+proto",
			status: http.StatusSwitchingProtocols,
			echoed: "connectrpc.1+proto",
		},
		{
			// The bare token names no codec, so it is not one of this
			// binding's tokens at all.
			name:   "the bare token is not a Connect subprotocol",
			codecs: []connect.Codec{connectproto.NewJSONCodec()},
			offer:  "connectrpc.1",
			status: http.StatusBadRequest,
		},
		{
			name:   "unrecognized tokens are not an encoding problem",
			codecs: []connect.Codec{connectproto.NewBinaryCodec()},
			offer:  "mqtt, chat",
			status: http.StatusBadRequest,
		},
		{
			name:   "no Sec-WebSocket-Protocol at all",
			codecs: []connect.Codec{connectproto.NewBinaryCodec()},
			status: http.StatusBadRequest,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(server, pingServer{})
			mux := http.NewServeMux()
			connecthttp.Mount(mux, server, connecthttp.WithCodecs(test.codecs...))
			httpServer := httptest.NewServer(mux)
			t.Cleanup(httpServer.Close)

			response := offerSubprotocol(t, httpServer, test.offer)
			defer func() { _ = response.Body.Close() }()
			assert.Equal(t, response.StatusCode, test.status)
			if test.echoed != "" {
				assert.Equal(t, response.Header.Get("Sec-WebSocket-Protocol"), test.echoed)
			}
		})
	}
}

// §4.3: a server that supports none of the offered tokens refuses the
// handshake with 400, even when it recognizes them. The body names what it
// does speak, for the non-browser clients and developer tools that can read it.
func TestUnsupportedCodecIsRejectedWithBadRequest(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		codecs []connect.Codec
		offer  string
		speaks string
	}{
		{
			// JSON is served even though the codecs name only proto (§4.3).
			name:   "proto server, client offering only a codec it lacks",
			codecs: []connect.Codec{connectproto.NewBinaryCodec()},
			offer:  "connectrpc.1+msgpack",
			speaks: "connectrpc.1+proto, connectrpc.1+json",
		},
		{
			name:   "JSON-only server, proto-only client",
			codecs: []connect.Codec{connectproto.NewJSONCodec()},
			offer:  "connectrpc.1+proto",
			speaks: "connectrpc.1+json",
		},
		{
			name: "custom codecs are listed too",
			codecs: []connect.Codec{
				connectproto.NewBinaryCodec(),
				connectproto.NewJSONCodec(),
				customTextCodec{connectproto.NewJSONCodec()},
			},
			offer:  "connectrpc.1+nope",
			speaks: "connectrpc.1+customtext",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(server, pingServer{})
			mux := http.NewServeMux()
			connecthttp.Mount(mux, server, connecthttp.WithCodecs(test.codecs...))
			httpServer := httptest.NewServer(mux)
			t.Cleanup(httpServer.Close)

			response := offerSubprotocol(t, httpServer, test.offer)
			defer func() { _ = response.Body.Close() }()
			assert.Equal(t, response.StatusCode, http.StatusBadRequest)
			body, readErr := io.ReadAll(response.Body)
			assert.Nil(t, readErr)
			assert.True(t, strings.Contains(string(body), test.speaks))
		})
	}
}

// §4.3: the client offers its preferred codec and then the other, and encodes
// with whichever the server echoes, and the call reports the codec actually
// used.
func TestClientFollowsServerCodecChoice(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		server    connect.Codec
		sendCodec string
		echoed    string
	}{
		{
			// §4.3: a server supports JSON whatever its codecs were narrowed
			// to, so the client's first choice is honored.
			name:      "client prefers JSON, server configured with only proto",
			server:    connectproto.NewBinaryCodec(),
			sendCodec: connect.CodecNameJSON,
			echoed:    "connectrpc.1+json",
		},
		{
			name:      "client prefers proto, server has only JSON",
			server:    connectproto.NewJSONCodec(),
			sendCodec: connect.CodecNameProto,
			echoed:    "connectrpc.1+json",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(server, pingServer{})
			mux := http.NewServeMux()
			connecthttp.Mount(mux, server, connecthttp.WithCodecs(test.server))
			httpServer := httptest.NewServer(mux)
			t.Cleanup(httpServer.Close)

			recorder := &recordingTransport{base: httpServer.Client().Transport}
			transport := connecthttp.NewTransport(
				&http.Client{Transport: recorder},
				httpServer.URL,
				connecthttp.WithWebSocket(connecthttp.SelectAll),
				connecthttp.WithSendCodec(test.sendCodec),
			)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
			ctx, info := connect.NewClientContext(t.Context())
			stream, err := client.CumSum(ctx)
			assert.Nil(t, err)
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 4}))
			sum, err := stream.Receive()
			assert.Nil(t, err)
			assert.Equal(t, sum.Sum, int64(4))
			assert.Nil(t, stream.CloseSend())
			assert.Nil(t, stream.Close())
			assert.Equal(t, *recorder.subprotocol.Load(), test.echoed)
			assert.Equal(t, info.Codec, strings.TrimPrefix(test.echoed, "connectrpc.1+"))
		})
	}
}

// firstOfferedSubprotocol is the token a hand-built server echoes: the first
// one the client offered, which is what negotiation picks against a server
// holding both codecs.
func firstOfferedSubprotocol(request *http.Request) string {
	offer, _, _ := strings.Cut(request.Header.Get("Sec-WebSocket-Protocol"), ",")
	return strings.TrimSpace(offer)
}

// offerSubprotocol sends one handshake with the given Sec-WebSocket-Protocol
// offer. A successful upgrade's body is the hijacked socket, so a caller that
// asserts on the status must not read it.
func offerSubprotocol(tb testing.TB, server *httptest.Server, offer string) *http.Response {
	tb.Helper()
	request, err := http.NewRequestWithContext(tb.Context(), http.MethodGet,
		server.URL+pingv1connect.PingServiceCumSumProcedure, nil)
	assert.Nil(tb, err)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	if offer != "" {
		request.Header.Set("Sec-WebSocket-Protocol", offer)
	}
	response, err := server.Client().Do(request)
	assert.Nil(tb, err)
	tb.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// The subprotocol names the encoding, and nothing on the wire stops a peer
// from sending the other one. A receiver that decoded it anyway would let the
// peer pick an encoding the handshake did not agree on — so this is refused
// whether or not the receiver holds a codec for what arrived.
func TestBodyInTheWrongEncodingIsRejected(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		codecs []connect.Codec
	}{
		{
			// The hole worth closing: the server has a JSON codec and could
			// decode this, but the connection negotiated proto.
			name:   "receiver holds the codec anyway",
			codecs: nil, // the default pair
		},
		{
			name:   "receiver does not hold the codec",
			codecs: []connect.Codec{connectproto.NewBinaryCodec()},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(server, pingServer{})
			mux := http.NewServeMux()
			var options []connecthttp.Option
			if test.codecs != nil {
				options = append(options, connecthttp.WithCodecs(test.codecs...))
			}
			connecthttp.Mount(mux, server, options...)
			httpServer := httptest.NewServer(mux)
			t.Cleanup(httpServer.Close)

			// dialCumSum negotiates +proto and has sent the opening M.
			conn := dialCumSum(t, httpServer, "")
			// A JSON body in a text frame, on a Protobuf connection.
			sendJSONMessage(t, conn, wireBody, []byte(`{"number":"1"}`))

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			assert.True(t, strings.Contains(string(data), "internal"))
			// §6.1: names the frame type that was expected.
			assert.True(t, strings.Contains(string(data),
				"body in a text frame; the negotiated proto codec expects a binary frame"))
		})
	}
}
