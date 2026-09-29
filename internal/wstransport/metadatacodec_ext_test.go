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
// unencoded. Base64 is what makes the round trip possible at all.
func TestBinaryMetadataSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()
	raw := string([]byte{0x00, 0xff, 0xfe, 0x80, 'h', 'i'})
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
	info.RequestHeader().SetValues("Acme-Token-Bin", []string{raw})
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	assert.Nil(t, stream.CloseSend())
	_, err = stream.Receive()
	assert.True(t, errors.Is(err, io.EOF))
	assert.Nil(t, stream.Close())

	assert.Equal(t, info.ResponseTrailer().Get("Acme-Echo-Bin"), raw)
}

// The wire form is what a second implementation has to agree with: lower-case
// keys, and -bin values in unpadded standard base64.
func TestMetadataWireForm(t *testing.T) {
	t.Parallel()
	raw := []byte{0x00, 0xff, 0xfe}
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, trailerWritingServer{
		key: "Acme-Echo-Bin", value: string(raw),
	})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
	sendJSONMessage(t, conn, wireMetadata, []byte(`{}`))
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

// A key differing only in case is the same key, so a client cannot use casing
// to slip past the handshake's precedence.
func TestMetadataKeysAreCaseInsensitive(t *testing.T) {
	t.Parallel()
	observations := make(chan string, 4)
	httpServer := newObservationServer(t, observations)
	conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
	sendJSONMessage(t, conn, wireMetadata, []byte(`{"acme-tenant":["lower-case-key"]}`))
	sendProtoBody(t, conn, &pingv1.CumSumRequest{Number: 1})

	// observationServer reads "Acme-Tenant"; the client sent "acme-tenant".
	assert.Equal(t, <-observations, "lower-case-key")
}

// A -bin value that is not base64 is a protocol error, not a value to pass
// through: a receiver that accepted it would hand the application bytes the
// sender never wrote.
func TestUndecodableBinaryMetadataIsRejected(t *testing.T) {
	t.Parallel()
	httpServer := newHybridServer(t, pingServer{})
	conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
	sendJSONMessage(t, conn, wireMetadata, []byte(`{"acme-token-bin":["not!base64"]}`))

	data := readServerOpening(t, conn)
	assert.Equal(t, data[0], wireServerEndStream)
	assert.True(t, strings.Contains(string(data), "invalid_argument"))
	assert.True(t, strings.Contains(string(data), "base64"))
}

// Senders must not pad, but a receiver has to take both: the padding is
// unambiguous, and refusing it fails an RPC over a value that decodes fine.
func TestPaddedBinaryMetadataIsAccepted(t *testing.T) {
	t.Parallel()
	raw := []byte{0x00, 0xff}
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, echoMetadataServer{
		readKey: "Acme-Token-Bin", writeKey: "Acme-Echo-Bin",
	})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	// StdEncoding, so the value carries "=" padding a sender should not emit.
	padded := base64.StdEncoding.EncodeToString(raw)
	assert.True(t, strings.HasSuffix(padded, "="))

	conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
	sendJSONMessage(t, conn, wireMetadata, []byte(`{"acme-token-bin":["`+padded+`"]}`))
	sendJSONMessage(t, conn, wireClientEndStream, nil)

	data := readServerOpening(t, conn)
	assert.Equal(t, data[0], wireServerEndStream)
	var end struct {
		Metadata map[string][]string `json:"metadata"`
	}
	assert.Nil(t, json.Unmarshal(data[1:], &end))
	echoed, ok := end.Metadata["acme-echo-bin"]
	assert.True(t, ok)
	assert.Equal(t, len(echoed), 1)
	// The handler saw the decoded bytes, so the echo re-encodes them unpadded.
	assert.Equal(t, echoed[0], base64.RawStdEncoding.EncodeToString(raw))
}

// Two keys that fold to one are one key, and a map would keep whichever it
// reached last — a choice neither peer can see and Go does not make twice the
// same way. It is a protocol error instead.
func TestDuplicateMetadataKeysAreRejected(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "differing only in case", payload: `{"acme-tenant":["a"],"Acme-Tenant":["b"]}`},
		{name: "spelled identically", payload: `{"acme-tenant":["a"],"acme-tenant":["b"]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer(t, pingServer{})
			conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
			sendJSONMessage(t, conn, wireMetadata, []byte(test.payload))

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			assert.True(t, strings.Contains(string(data), "invalid_argument"))
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
			payload: `{"acme tenant":["x"]}`,
			mention: "field name",
		},
		{
			name:    "separator in the key",
			payload: `{"acme:tenant":["x"]}`,
			mention: "field name",
		},
		{
			name:    "empty key",
			payload: `{"":["x"]}`,
			mention: "field name",
		},
		{
			// A header injection wherever this is forwarded as HTTP.
			name:    "CRLF in the value",
			payload: `{"acme-tenant":["a\r\nX-Evil: 1"]}`,
			mention: "field value",
		},
		{
			name:    "NUL in the value",
			payload: `{"acme-tenant":["a\u0000b"]}`,
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
			assert.True(t, strings.Contains(string(data), "invalid_argument"))
			assert.True(t, strings.Contains(string(data), test.mention))
		})
	}
}

// A -bin value holds arbitrary bytes, so the field-value rule cannot apply to
// it: base64 is what carries them, and what it decodes to is not a field value.
func TestBinaryMetadataMayDecodeToAnyBytes(t *testing.T) {
	t.Parallel()
	raw := string([]byte{'\r', '\n', 0x00, 0xff})
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, echoMetadataServer{
		readKey: "Acme-Token-Bin", writeKey: "Acme-Echo-Bin",
	})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	conn := dialRaw(t, httpServer, pingv1connect.PingServiceCumSumProcedure)
	encoded := base64.RawStdEncoding.EncodeToString([]byte(raw))
	sendJSONMessage(t, conn, wireMetadata, []byte(`{"acme-token-bin":["`+encoded+`"]}`))
	sendJSONMessage(t, conn, wireClientEndStream, nil)

	data := readServerOpening(t, conn)
	assert.Equal(t, data[0], wireServerEndStream)
	var end struct {
		Metadata map[string][]string `json:"metadata"`
	}
	assert.Nil(t, json.Unmarshal(data[1:], &end))
	assert.Equal(t, end.Metadata["acme-echo-bin"], []string{encoded})
}

// An M with no payload is not an empty object; the empty object is {}.
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
