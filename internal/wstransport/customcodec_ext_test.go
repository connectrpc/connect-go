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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"google.golang.org/protobuf/proto"
)

// customBinaryCodec is Protobuf binary under a name of its own, so it takes
// the default binary frame type.
type customBinaryCodec struct {
	*connectproto.BinaryCodec
}

func (customBinaryCodec) Name() string { return "custom" }

// customTextCodec is Protobuf JSON under a name of its own, declared as text.
type customTextCodec struct {
	*connectproto.JSONCodec
}

func (customTextCodec) Name() string { return "customtext" }
func (customTextCodec) IsText() bool { return true }

// invalidUTF8Codec claims to be text but writes a byte that is never UTF-8.
type invalidUTF8Codec struct {
	*connectproto.JSONCodec
}

func (invalidUTF8Codec) Name() string { return "badtext" }
func (invalidUTF8Codec) IsText() bool { return true }
func (invalidUTF8Codec) MarshalWrite(_ context.Context, dst io.Writer, _ any) error {
	_, err := dst.Write([]byte{0xff})
	return err
}

var (
	_ connecthttp.TextCodec = customTextCodec{}
	_ connecthttp.TextCodec = invalidUTF8Codec{}
)

// newCustomCodecServer serves Ping and CumSum with the default codecs and
// the given extra ones.
func newCustomCodecServer(tb testing.TB, extra ...connect.Codec) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	codecs := append([]connect.Codec{connectproto.NewBinaryCodec(), connectproto.NewJSONCodec()}, extra...)
	connecthttp.Mount(mux, server, connecthttp.WithCodecs(codecs...))
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// §4.3: a codec other than proto or json travels as connectrpc.1+ and its
// name, and §6.1: its bodies use the frame type its definition states.
func TestCustomCodecRoundTrips(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		codec connect.Codec
		text  bool
	}{
		{codec: customBinaryCodec{connectproto.NewBinaryCodec()}, text: false},
		{codec: customTextCodec{connectproto.NewJSONCodec()}, text: true},
	} {
		t.Run(test.codec.Name(), func(t *testing.T) {
			t.Parallel()
			httpServer := newCustomCodecServer(t, test.codec)

			transport := connecthttp.NewTransport(
				httpServer.Client(),
				httpServer.URL,
				connecthttp.WithWebSocket(connecthttp.SelectAll),
				connecthttp.WithCodecs(test.codec),
			)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
			ctx, info := connect.NewClientContext(t.Context())
			response, err := client.Ping(ctx, &pingv1.PingRequest{Number: 7})
			assert.Nil(t, err)
			assert.Equal(t, response.Number, int64(7))
			assert.Equal(t, info.Codec, test.codec.Name())

			// On the wire, the body goes in the codec's frame type both ways.
			browser := dialBrowserClientWithToken(
				t, httpServer, pingv1connect.PingServicePingProcedure, "connectrpc.1+"+test.codec.Name(),
			)
			var payload []byte
			if test.text {
				payload = []byte(`{"number":"7"}`)
			} else {
				payload, err = proto.Marshal(&pingv1.PingRequest{Number: 7})
				assert.Nil(t, err)
			}
			browser.writeMessage(t, wireBody, test.text, payload)
			browser.writeJSON(t, wireClientEndStream, nil)
			marker, _, _ := browser.readMessage(t)
			assert.Equal(t, marker, wireMetadata)
			marker, _, text := browser.readMessage(t)
			assert.Equal(t, marker, wireBody)
			assert.Equal(t, text, test.text)
			marker, endStream, _ := browser.readMessage(t)
			assert.Equal(t, marker, wireServerEndStream)
			assert.False(t, strings.Contains(string(endStream), "error"))
		})
	}
}

// A client offers its other codecs after its send codec, so a server without
// the custom codec answers in one they share and the client follows it.
func TestClientFallsBackFromCustomCodec(t *testing.T) {
	t.Parallel()
	httpServer := newCustomCodecServer(t)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
		connecthttp.WithCodecs(customBinaryCodec{connectproto.NewBinaryCodec()}, connectproto.NewJSONCodec()),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	ctx, info := connect.NewClientContext(t.Context())
	response, err := client.Ping(ctx, &pingv1.PingRequest{Number: 3})
	assert.Nil(t, err)
	assert.Equal(t, response.Number, int64(3))
	assert.Equal(t, info.Codec, connect.CodecNameJSON)
}

// Regression: without WithSendCodec, a client sends with the first codec given
// to WithCodecs, over WebSocket as over HTTP. WebSocket used to default to
// proto regardless.
func TestFirstRegisteredCodecIsTheDefaultSendCodec(t *testing.T) {
	t.Parallel()
	httpServer := newCustomCodecServer(t)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
		connecthttp.WithCodecs(connectproto.NewJSONCodec(), connectproto.NewBinaryCodec()),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	ctx, info := connect.NewClientContext(t.Context())
	_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 1})
	assert.Nil(t, err)
	assert.Equal(t, info.Codec, connect.CodecNameJSON)
}

// §6.1: a text codec must produce valid UTF-8. A body that is not would make
// the peer fail the connection with 1007, so the sender refuses it instead.
func TestTextCodecMustProduceUTF8(t *testing.T) {
	t.Parallel()
	codec := invalidUTF8Codec{connectproto.NewJSONCodec()}
	httpServer := newCustomCodecServer(t, codec)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
		connecthttp.WithCodecs(codec),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
	if !assert.NotNil(t, err) {
		return
	}
	assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
	assert.True(t, strings.Contains(err.Error(), `codec "badtext" produced invalid UTF-8`))
}

// Regression: a send codec whose name no token can carry used to be offered
// as connectrpc.1+proto. It is refused before anything is dialed.
func TestUncarriableSendCodecIsRefusedBeforeDialing(t *testing.T) {
	t.Parallel()
	var upgrades atomic.Int32
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(http.HandlerFunc(
		func(responseWriter http.ResponseWriter, request *http.Request) {
			if connecthttp.IsWebSocketUpgrade(request) {
				upgrades.Add(1)
			}
			mux.ServeHTTP(responseWriter, request)
		},
	))
	t.Cleanup(httpServer.Close)

	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
		connecthttp.WithCodecs(upperCaseCodec{connectproto.NewBinaryCodec()}, connectproto.NewBinaryCodec()),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	_, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 1})
	if !assert.NotNil(t, err) {
		return
	}
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnknown)
	assert.True(t, strings.Contains(err.Error(), `send codec "Custom" has no WebSocket subprotocol`))
	assert.Equal(t, upgrades.Load(), int32(0))
}

// upperCaseCodec has a name §4.3 does not allow in a token.
type upperCaseCodec struct {
	*connectproto.BinaryCodec
}

func (upperCaseCodec) Name() string { return "Custom" }
