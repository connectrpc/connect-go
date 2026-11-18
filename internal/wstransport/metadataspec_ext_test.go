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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/wstransport"
	"google.golang.org/protobuf/proto"
)

// headerSettingServer sets the given response headers, optionally flushing
// them before setting one more, and answers with one message.
type headerSettingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	headers   http.Header
	flush     bool
	lateKey   string
	requested chan http.Header
}

func (s headerSettingServer) CountUp(
	ctx context.Context,
	_ *pingv1.CountUpRequest,
	stream pingv1connect.PingServiceCountUpServerStream,
) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	if s.requested != nil {
		requested := make(http.Header)
		for key, values := range info.RequestHeader().All() {
			requested[http.CanonicalHeaderKey(key)] = values
		}
		s.requested <- requested
	}
	for key, values := range s.headers {
		info.ResponseHeader().SetValues(key, values)
	}
	if s.flush {
		if err := stream.SendHeaders(); err != nil {
			return err
		}
		info.ResponseHeader().Set(s.lateKey, "late")
	}
	return stream.Send(&pingv1.CountUpResponse{Number: 1})
}

func openCountUp(tb testing.TB, httpServer *httptest.Server, metadata string) *browserClient {
	tb.Helper()
	client := dialBrowserClientOpening(tb, httpServer, pingv1connect.PingServiceCountUpProcedure, []byte(metadata))
	payload, err := proto.Marshal(&pingv1.CountUpRequest{Number: 1})
	assert.Nil(tb, err)
	client.writeProto(tb, payload)
	client.writeJSON(tb, wireClientEndStream, nil)
	return client
}

// §7.3.1 binds the sender: values are trimmed, and -bin values are unpadded.
func TestServerSendsTrimmedUnpaddedMetadata(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, headerSettingServer{headers: http.Header{
		"Acme-Bin":    {"YQ=="},
		"Acme-Spaced": {"  value \t"},
	}})
	client := openCountUp(t, httpServer, "[]")
	marker, metadata, _ := client.readMessage(t)
	assert.Equal(t, marker, wireMetadata)
	assert.Equal(t, string(metadata), `[["acme-bin","YQ"],["acme-spaced","value"]]`)
}

// §7.3.1: a header the server may not send is not sent. M still opens the
// stream, empty, and the RPC fails naming the header.
func TestServerRefusesToSendInvalidResponseHeader(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]string{
		"control character": "a\nb",
		"non-ASCII":         "café",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			httpServer := newServerFor(t, headerSettingServer{headers: http.Header{"Acme-Bad": {value}}})
			client := openCountUp(t, httpServer, "[]")
			marker, metadata, _ := client.readMessage(t)
			assert.Equal(t, marker, wireMetadata)
			assert.Equal(t, string(metadata), "[]")
			marker, endStream, _ := client.readMessage(t)
			assert.Equal(t, marker, wireServerEndStream)
			assert.True(t, strings.Contains(string(endStream), `"code":"internal"`))
			assert.True(t, strings.Contains(string(endStream), "Acme-Bad"))
		})
	}
}

// §7.4: an explicit flush sends M then, and a header set after it is late and
// not sent.
func TestSendHeadersFlushesLeadingMetadata(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, headerSettingServer{
		headers: http.Header{"Acme-Early": {"early"}},
		flush:   true,
		lateKey: "Acme-Late",
	})
	client := openCountUp(t, httpServer, "[]")
	marker, metadata, _ := client.readMessage(t)
	assert.Equal(t, marker, wireMetadata)
	assert.Equal(t, string(metadata), `[["acme-early","early"]]`)
	marker, _, _ = client.readMessage(t)
	assert.Equal(t, marker, wireBody)
	marker, endStream, _ := client.readMessage(t)
	assert.Equal(t, marker, wireServerEndStream)
	assert.False(t, strings.Contains(string(endStream), "late"))
}

// §7.3.1: a receiver trims every value before delivering it, -bin ones too.
func TestReceiverTrimsBinaryValues(t *testing.T) {
	t.Parallel()
	requested := make(chan http.Header, 1)
	httpServer := newServerFor(t, headerSettingServer{requested: requested})
	_ = openCountUp(t, httpServer, `[["acme-bin"," YQ \t"],["acme-text"," v "]]`)
	header := <-requested
	assert.Equal(t, header.Get("Acme-Bin"), "YQ")
	assert.Equal(t, header.Get("Acme-Text"), "v")
}

// §7.3.2 and §7.3.1 bind a client's M too. A header the client may not send
// fails the call before it dials, rather than as the server's protocol error.
func TestClientRefusesRequestHeadersItMayNotSend(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	mountBoth(mux, server)
	var upgrades atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if wstransport.IsUpgrade(request) {
			upgrades.Add(1)
		}
		mux.ServeHTTP(writer, request)
	}))
	t.Cleanup(httpServer.Close)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	for _, test := range []struct {
		key, value, mention string
	}{
		{key: "Cookie", value: "session=1", mention: "Fetch"},
		{key: "Connect-Acme", value: "1", mention: "protocol"},
		{key: "X-Forwarded-For", value: "10.0.0.1", mention: "deny list"},
		{key: "Acme-Bad", value: "a\nb", mention: "valid HTTP field value"},
		{key: "Acme-Bin", value: "not base64!", mention: "base64"},
	} {
		ctx, info := connect.NewClientContext(t.Context())
		info.RequestHeader().Set(test.key, test.value)
		_, err := client.Ping(ctx, &pingv1.PingRequest{Number: 1})
		assert.Equal(t, connect.CodeOf(err), connect.CodeInternal, assert.Sprintf("header %s", test.key))
		assert.True(t, err != nil && strings.Contains(err.Error(), test.mention), assert.Sprintf("%v", err))
	}
	assert.Equal(t, upgrades.Load(), int32(0))
}
