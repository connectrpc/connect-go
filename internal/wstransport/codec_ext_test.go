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

// A bare C has no payload to encode, so nothing about it implies text. §6.1
// gives it the negotiated frame type, which is the only thing left to say what
// the connection agreed on.
func TestBareEndOfClientStreamFollowsTheNegotiatedFrameType(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		codec    string
		wantText bool
	}{
		{codec: connect.CodecNameProto},
		{codec: connect.CodecNameJSON, wantText: true},
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

			assert.Equal(t, readFrameType(t, frames), true)          // opening M is JSON
			assert.Equal(t, readFrameType(t, frames), test.wantText) // bare C is negotiated
		})
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
				Subprotocols: []string{request.Header.Get("Sec-WebSocket-Protocol")},
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
		name    string
		codecs  []connect.Codec
		offer   string
		status  int
		echoed  string
		mention string
	}{
		{
			name:   "proto-only server takes the client's second choice",
			codecs: []connect.Codec{connectproto.NewBinaryCodec()},
			// JSON first: a server that failed on the first recognized token
			// would refuse a client it can in fact serve.
			offer:  "connectrpc.1+json, connectrpc.1+proto",
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
			name:   "an unrecognized token is not an encoding problem",
			codecs: []connect.Codec{connectproto.NewBinaryCodec()},
			offer:  "mqtt",
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
			assert.Equal(t, response.StatusCode, test.status)
			if test.echoed != "" {
				assert.Equal(t, response.Header.Get("Sec-WebSocket-Protocol"), test.echoed)
				return
			}
			body, readErr := io.ReadAll(response.Body)
			assert.Nil(t, readErr)
			assert.Nil(t, response.Body.Close())
			if test.mention != "" {
				assert.True(t, strings.Contains(string(body), test.mention))
			}
		})
	}
}

// A client that offered a Connect token is speaking this protocol; it just
// named a codec this server lacks. That is an RPC failure, not a connection
// failure, so the upgrade completes and the verdict travels in band — the only
// channel a browser script can read.
func TestUnsupportedCodecIsReportedInBand(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		codecs []connect.Codec
		offer  string
		speaks string
	}{
		{
			name:   "proto-only server, JSON-only client",
			codecs: []connect.Codec{connectproto.NewBinaryCodec()},
			offer:  "connectrpc.1+json",
			speaks: "proto",
		},
		{
			name:   "JSON-only server, proto-only client",
			codecs: []connect.Codec{connectproto.NewJSONCodec()},
			offer:  "connectrpc.1+proto",
			speaks: "json",
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

			url := "ws" + strings.TrimPrefix(httpServer.URL, "http") +
				pingv1connect.PingServiceCumSumProcedure
			conn, response, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{
				HTTPClient:   httpServer.Client(),
				Subprotocols: []string{test.offer},
			})
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			// The handshake succeeds, and the token comes back as offered.
			assert.Nil(t, err)
			t.Cleanup(func() { _ = conn.CloseNow() })
			assert.Equal(t, response.StatusCode, http.StatusSwitchingProtocols)
			assert.Equal(t, response.Header.Get("Sec-WebSocket-Protocol"), test.offer)

			// Every response stream opens with M, even this one.
			readCtx, cancelRead := readContext(t)
			defer cancelRead()
			_, opening, err := conn.Read(readCtx)
			assert.Nil(t, err)
			assert.Equal(t, string(opening), "M{}")

			_, data, err := conn.Read(readCtx)
			assert.Nil(t, err)
			assert.Equal(t, data[0], wireServerEndStream)
			assert.True(t, strings.Contains(string(data), "unimplemented"))
			// The message names what to offer instead.
			assert.True(t, strings.Contains(string(data), test.speaks))

			// The RPC reached a verdict, so the close is ordinary.
			_, _, err = conn.Read(readCtx)
			assert.Equal(t, websocket.CloseStatus(err), websocket.StatusNormalClosure)
		})
	}
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
	request.Header.Set("Sec-WebSocket-Protocol", offer)
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
			assert.True(t, strings.Contains(string(data), "invalid_argument"))
			// Names both encodings: which arrived, and which was agreed.
			assert.True(t, strings.Contains(string(data), "json body on a proto connection"))
		})
	}
}
