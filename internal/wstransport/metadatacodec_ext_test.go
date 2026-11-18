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
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// echoMetadataServer reports one request header back as a trailer, so a test
// can watch a value make the round trip.
type echoMetadataServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	readKey  string
	writeKey string
}

func (s echoMetadataServer) CumSum(
	ctx context.Context,
	stream pingv1connect.PingServiceCumSumServerStream,
) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	info.ResponseTrailer().SetValues(s.writeKey, info.RequestHeader().Values(s.readKey))
	for {
		if _, err := stream.Receive(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// A -bin value carries bytes that are not valid UTF-8, which JSON cannot hold
// unencoded. As over HTTP, the application holds the base64 form, and the
// transport carries it unchanged in both directions.
func TestBinaryMetadataSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()
	raw := []byte{0x00, 0xff, 0xfe, 0x80, 'h', 'i'}
	encoded := connect.EncodeBinaryHeader(raw)
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, echoMetadataServer{
		readKey: "Acme-Token-Bin", writeKey: "Acme-Echo-Bin",
	})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectStreaming),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	ctx, info := connect.NewClientContext(t.Context())
	info.RequestHeader().SetValues("Acme-Token-Bin", []string{encoded})
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	assert.Nil(t, stream.CloseSend())
	_, err = stream.Receive()
	assert.True(t, errors.Is(err, io.EOF))
	assert.Nil(t, stream.Close())

	echoed := info.ResponseTrailer().Get("Acme-Echo-Bin")
	assert.Equal(t, echoed, encoded)
	decoded, err := connect.DecodeBinaryHeader(echoed)
	assert.Nil(t, err)
	assert.Equal(t, decoded, raw)
}

// The wire form is what a second implementation has to agree with: lower-case
// keys, and -bin values exactly as the application encoded them.
func TestMetadataWireForm(t *testing.T) {
	t.Parallel()
	raw := []byte{0x00, 0xff, 0xfe}
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, trailerWritingServer{
		key: "Acme-Echo-Bin", value: connect.EncodeBinaryHeader(raw),
	})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
	sendJSONMessage(t, conn, wireMetadata, []byte(`[]`))
	sendJSONMessage(t, conn, wireClientEndStream, nil)

	data := readServerOpening(t, conn)
	assert.Equal(t, data[0], wireServerEndStream)

	var end struct {
		Metadata map[string][]string `json:"metadata"`
	}
	assert.Nil(t, json.Unmarshal(data[1:], &end))
	values, ok := end.Metadata["acme-echo-bin"]
	assert.True(t, ok) // lower-case on the wire, not canonical MIME case
	assert.Equal(t, len(values), 1)
	assert.Equal(t, values[0], base64.RawStdEncoding.EncodeToString(raw))
}

// trailerWritingServer sets one trailer and returns.
type trailerWritingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	key   string
	value string
}

func (s trailerWritingServer) CumSum(
	ctx context.Context,
	_ pingv1connect.PingServiceCumSumServerStream,
) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	info.ResponseTrailer().SetValues(s.key, []string{s.value})
	return nil
}

// §7.3.1: keys are lower-case on the wire, as in HTTP/2, so a key with an
// upper-case character is a protocol error rather than a second spelling.
func TestUpperCaseMetadataKeyIsRejected(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"Acme-Tenant", "ACME-TENANT", "acme-Tenant"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer(t, pingServer{})
			conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
			sendJSONMessage(t, conn, wireMetadata, []byte(`[["`+key+`","x"]]`))

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			assert.True(t, strings.Contains(string(data), "internal"))
			assert.True(t, strings.Contains(string(data), "not lower-case"))
		})
	}
}

// §7.3.1: as over HTTP, a -bin value is not decoded on arrival. It reaches the
// application as sent, and fails, if at all, only when decoded there.
func TestBinaryMetadataPassesThroughUndecoded(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value string
	}{
		{
			// Senders must not pad, but the padding is unambiguous, and refusing
			// it would fail an RPC over a value that decodes fine.
			name:  "padded",
			value: base64.StdEncoding.EncodeToString([]byte{0x00, 0xff}),
		},
		{name: "not base64", value: "not!base64"},
		{
			// What a -bin value decodes to is not a field value, so the
			// field-value rule cannot apply to it.
			name:  "decodes to bytes no field value may hold",
			value: base64.RawStdEncoding.EncodeToString([]byte{'\r', '\n', 0x00, 0xff}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(server, echoMetadataServer{
				readKey: "Acme-Token-Bin", writeKey: "Acme-Echo",
			})
			mux := http.NewServeMux()
			connecthttp.Mount(mux, server)
			httpServer := httptest.NewServer(mux)
			t.Cleanup(httpServer.Close)

			conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
			sendJSONMessage(t, conn, wireMetadata, []byte(`[["acme-token-bin","`+test.value+`"]]`))
			sendJSONMessage(t, conn, wireClientEndStream, nil)

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			var end struct {
				Error    json.RawMessage     `json:"error"`
				Metadata map[string][]string `json:"metadata"`
			}
			assert.Nil(t, json.Unmarshal(data[1:], &end))
			assert.Equal(t, len(end.Error), 0)
			assert.Equal(t, end.Metadata["acme-echo"], []string{test.value})
		})
	}
}

// Two entries whose keys fold to one would leave the receiver to choose between
// them, a choice the sender cannot see. It is a protocol error instead.
func TestDuplicateMetadataKeysAreRejected(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "differing only in case", payload: `[["acme-tenant","a"],["Acme-Tenant","b"]]`},
		{name: "spelled identically", payload: `[["acme-tenant","a"],["acme-tenant","b"]]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer(t, pingServer{})
			conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
			sendJSONMessage(t, conn, wireMetadata, []byte(test.payload))

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			assert.True(t, strings.Contains(string(data), "internal"))
			assert.True(t, strings.Contains(string(data), "same key"))
		})
	}
}

// Metadata stands in for HTTP headers and may be forwarded as one, so a key
// that is not a field name, or a value carrying the bytes that end a header
// line, cannot be passed through.
func TestMalformedMetadataNamesAndValuesAreRejected(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		payload string
		mention string
	}{
		{
			name:    "space in the key",
			payload: `[["acme tenant","x"]]`,
			mention: "field name",
		},
		{
			name:    "separator in the key",
			payload: `[["acme:tenant","x"]]`,
			mention: "field name",
		},
		{
			name:    "empty key",
			payload: `[["","x"]]`,
			mention: "field name",
		},
		{
			// A header injection wherever this is forwarded as HTTP.
			name:    "CRLF in the value",
			payload: `[["acme-tenant","a\r\nX-Evil: 1"]]`,
			mention: "field value",
		},
		{
			name:    "NUL in the value",
			payload: `[["acme-tenant","a\u0000b"]]`,
			mention: "field value",
		},
		{
			// RFC 9110 allows obs-text, but a JSON string has no byte form for it.
			name:    "non-ASCII in the value",
			payload: `[["acme-tenant","café"]]`,
			mention: "field value",
		},
		{
			name:    "DEL in the value",
			payload: `[["acme-tenant","a\u007fb"]]`,
			mention: "field value",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer(t, pingServer{})
			conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
			sendJSONMessage(t, conn, wireMetadata, []byte(test.payload))

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			assert.True(t, strings.Contains(string(data), "internal"))
			assert.True(t, strings.Contains(string(data), test.mention))
		})
	}
}

// An entry is an array of strings, the key first. Anything else, including the
// JSON object an earlier draft used, is a protocol error.
func TestMalformedMetadataEntriesAreRejected(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		payload string
		mention string
	}{
		{name: "object, not array", payload: `{"acme-tenant":["a"]}`, mention: "unmarshal M message"},
		{name: "null, not array", payload: `null`, mention: "M message is null"},
		{name: "entry is not an array", payload: `["acme-tenant"]`, mention: "unmarshal M message"},
		{name: "entry has no key", payload: `[[]]`, mention: "metadata entry is empty"},
		{name: "value is not a string", payload: `[["acme-tenant",1]]`, mention: "unmarshal M message"},
		{name: "values nested in an array", payload: `[["acme-tenant",["a"]]]`, mention: "unmarshal M message"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer(t, pingServer{})
			conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
			sendJSONMessage(t, conn, wireMetadata, []byte(test.payload))

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			assert.True(t, strings.Contains(string(data), test.mention))
		})
	}
}

// One entry carries every value of its key. §7.3.1 gives every key at least
// one value, and an empty string is one. Leading and trailing whitespace is
// not part of a value, as in RFC 9110 §5.5.
func TestMetadataEntryCarriesAllItsValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		payload string
		want    []string
	}{
		{name: "several values", payload: `[["acme-tenant","a","b"]]`, want: []string{"a", "b"}},
		{name: "one empty value", payload: `[["acme-tenant",""]]`, want: []string{""}},
		{name: "tab inside a value", payload: `[["acme-tenant","a\tb"]]`, want: []string{"a\tb"}},
		{name: "surrounding whitespace", payload: `[["acme-tenant"," \ta b\t ","c "]]`, want: []string{"a b", "c"}},
		{name: "whitespace only", payload: `[["acme-tenant"," \t "]]`, want: []string{""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := connect.NewServer()
			pingv1connect.RegisterPingServiceHandler(server, echoMetadataServer{
				readKey: "Acme-Tenant", writeKey: "Acme-Echo",
			})
			mux := http.NewServeMux()
			connecthttp.Mount(mux, server)
			httpServer := httptest.NewServer(mux)
			t.Cleanup(httpServer.Close)

			conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
			sendJSONMessage(t, conn, wireMetadata, []byte(test.payload))
			sendJSONMessage(t, conn, wireClientEndStream, nil)

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			var end struct {
				Error    json.RawMessage     `json:"error"`
				Metadata map[string][]string `json:"metadata"`
			}
			assert.Nil(t, json.Unmarshal(data[1:], &end))
			assert.Equal(t, len(end.Error), 0)
			assert.Equal(t, len(end.Metadata["acme-echo"]), len(test.want))
			for index, value := range test.want {
				assert.Equal(t, end.Metadata["acme-echo"][index], value)
			}
		})
	}
}

// §7.3.1: an entry that holds only a key is a protocol error. A sender that
// means "present but empty" sends an empty string.
func TestMetadataKeyWithoutValuesIsRejected(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
	sendJSONMessage(t, conn, wireMetadata, []byte(`[["acme-tenant"]]`))

	data := readServerOpening(t, conn)
	assert.Equal(t, data[0], wireServerEndStream)
	assert.True(t, strings.Contains(string(data), "internal"))
	assert.True(t, strings.Contains(string(data), "has no values"))
}

// A sender puts every value of a key in one entry, with the key lower-case.
func TestLeadingMetadataWireForm(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, leadingHeaderServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
	sendJSONMessage(t, conn, wireMetadata, []byte(`[]`))
	sendJSONMessage(t, conn, wireClientEndStream, nil)

	ctx, cancel := readContext(t)
	defer cancel()
	_, opening, err := conn.Read(ctx)
	assert.Nil(t, err)
	assert.Equal(t, string(opening), `M[["acme-tenant","a","b"],["acme-zone","z"]]`)
}

// leadingHeaderServer sets response headers and returns.
type leadingHeaderServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (leadingHeaderServer) CumSum(
	ctx context.Context,
	_ pingv1connect.PingServiceCumSumServerStream,
) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	info.ResponseHeader().SetValues("Acme-Zone", []string{"z"})
	info.ResponseHeader().SetValues("Acme-Tenant", []string{"a", "b"})
	return nil
}

// An M with no payload is not an empty array; empty metadata is [].
func TestBareMetadataMarkerIsRejected(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
	sendJSONMessage(t, conn, wireMetadata, nil)

	data := readServerOpening(t, conn)
	assert.Equal(t, data[0], wireServerEndStream)
	assert.True(t, strings.Contains(string(data), "empty M message"))
}

// Request metadata travels in the M message and nowhere else. A client that
// also put it on the handshake would shadow its own message, since the
// handshake wins on a key collision.
func TestRequestMetadataDoesNotRideTheHandshake(t *testing.T) {
	t.Parallel()
	handshake := make(chan http.Header, 1)
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case handshake <- request.Header.Clone():
		default:
		}
	}))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	ctx, info := connect.NewClientContext(t.Context())
	info.RequestHeader().SetValues("Acme-Tenant", []string{"t1"})
	// The handshake fails — this server never upgrades — but it is the request
	// that matters here.
	_, _ = client.Ping(ctx, &pingv1.PingRequest{Number: 1})

	select {
	case header := <-handshake:
		assert.Equal(t, header.Get("Acme-Tenant"), "")
		// The transport's own headers are still there.
		assert.True(t, header.Get("Sec-WebSocket-Protocol") != "")
	case <-time.After(5 * time.Second):
		t.Fatal("the client never sent a handshake")
	}
}
