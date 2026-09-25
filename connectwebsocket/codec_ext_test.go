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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
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
		// The bare token is defined to mean JSON.
		{name: "base token", token: "connectrpc.1", json: true},
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
			name:    "proto-only server refuses a JSON-only client",
			codecs:  []connect.Codec{connectproto.NewBinaryCodec()},
			offer:   "connectrpc.1+json",
			status:  http.StatusUnsupportedMediaType,
			mention: "proto",
		},
		{
			name:    "JSON-only server refuses a proto-only client",
			codecs:  []connect.Codec{connectproto.NewJSONCodec()},
			offer:   "connectrpc.1+proto",
			status:  http.StatusUnsupportedMediaType,
			mention: "json",
		},
		{
			name:   "JSON-only server serves the base token",
			codecs: []connect.Codec{connectproto.NewJSONCodec()},
			offer:  "connectrpc.1",
			status: http.StatusSwitchingProtocols,
			echoed: "connectrpc.1",
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
			connectwebsocket.Mount(mux, server, connectwebsocket.WithCodecs(test.codecs...))
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

// Negotiation never hands a peer a codec this end lacks, but nothing on the
// wire stops it from sending one anyway. That must be a protocol error naming
// the encoding, not a nil codec dereferenced inside the read path.
func TestBodyInAnUnsupportedEncodingIsRejected(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server,
		connectwebsocket.WithCodecs(connectproto.NewBinaryCodec()))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	// dialCumSum has already sent the opening M message.
	conn := dialCumSum(t, httpServer, "")
	// A JSON body against a server that holds no JSON codec.
	sendJSONMessage(t, conn, wireBody, []byte(`{"number":"1"}`))

	data := readServerOpening(t, conn)
	assert.Equal(t, rune(data[0]), wireServerEndStream)
	assert.True(t, strings.Contains(string(data), "invalid_argument"))
	assert.True(t, strings.Contains(string(data), "not configured to decode"))
	assert.True(t, strings.Contains(string(data), "json"))
}
