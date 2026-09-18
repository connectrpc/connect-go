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
	"strconv"
	"strings"
	"testing"

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
		{name: "proto", token: "connect.v2+proto"},
		{name: "json", token: "connect.v2+json", json: true},
		// The bare token is defined to mean proto.
		{name: "base token", token: "connect.v2"},
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
			client.writeEnvelope(t, 0, payload)
			client.writeEnvelope(t, flagEndClientStream, nil)

			flags, body := client.readEnvelope(t)
			assert.Equal(t, flags, uint8(0))
			if test.json {
				assertProtoJSON(t, body, value)
			} else {
				assertProtoBinary(t, body, value)
			}

			// The terminal envelope is JSON whichever codec carries the
			// messages: it is protocol, not payload. A client built for the
			// proto subprotocol still has to parse JSON here.
			flags, body = client.readEnvelope(t)
			assert.Equal(t, flags, uint8(flagEndStream))
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
