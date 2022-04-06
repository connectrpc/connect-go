// Copyright 2021-2022 Buf Technologies, Inc.
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

package connect_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	"connectrpc.com/connect/internal/gen/connect/connect/ping/v1/pingv1connect"
	pingv1 "connectrpc.com/connect/internal/gen/go/connect/ping/v1"
)

func BenchmarkConnect(b *testing.B) {
	mux := http.NewServeMux()
	mux.Handle(
		pingv1connect.NewPingServiceHandler(
			&ExamplePingServer{},
		),
	)
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	httpClient := server.Client()
	httpTransport, ok := httpClient.Transport.(*http.Transport)
	assert.True(b, ok)
	httpTransport.DisableCompression = true

	client, err := pingv1connect.NewPingServiceClient(
		httpClient,
		server.URL,
		connect.WithGRPC(),
		connect.WithGzip(),
		connect.WithGzipRequests(),
	)
	assert.Nil(b, err)
	pingRequest := &pingv1.PingRequest{Number: 42}
	b.ResetTimer()

	b.Run("unary", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = client.Ping(
					context.Background(),
					connect.NewRequest(pingRequest),
				)
			}
		})
	})
}

type ping struct {
	Number int `json:"number"`
}

func BenchmarkREST(b *testing.B) {
	handler := func(writer http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		defer func() {
			_, err := io.Copy(io.Discard, req.Body)
			assert.Nil(b, err)
		}()
		writer.Header().Set("Content-Type", "application/json")
		var body io.Reader = req.Body
		if req.Header.Get("Content-Encoding") == "gzip" {
			gr, err := gzip.NewReader(body)
			if err != nil {
				b.Fatalf("get gzip reader: %v", err)
			}
			defer gr.Close()
			body = gr
		}
		var out io.Writer = writer
		if strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") {
			writer.Header().Set("Content-Encoding", "gzip")
			gw := gzip.NewWriter(writer)
			defer gw.Close()
			out = gw
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			b.Fatalf("read body: %v", err)
		}
		var pingReq ping
		if err := json.Unmarshal(raw, &pingReq); err != nil {
			b.Fatalf("json unmarshal: %v", err)
		}
		bs, err := json.Marshal(&pingReq)
		if err != nil {
			b.Fatalf("json marshal: %v", err)
		}
		_, err = out.Write(bs)
		assert.Nil(b, err)
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(handler))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	b.ResetTimer()

	b.Run("unary", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				unaryRESTIteration(b, server.Client(), server.URL)
			}
		})
	})
}

func unaryRESTIteration(b *testing.B, client *http.Client, url string) {
	b.Helper()
	rawRequestBody := bytes.NewBuffer(nil)
	compressedRequestBody := gzip.NewWriter(rawRequestBody)
	encoder := json.NewEncoder(compressedRequestBody)
	if err := encoder.Encode(&ping{42}); err != nil {
		b.Fatalf("marshal request: %v", err)
	}
	compressedRequestBody.Close()
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		url,
		rawRequestBody,
	)
	if err != nil {
		b.Fatalf("construct request: %v", err)
	}
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("Accept-Encoding", "gzip")
	request.Header.Set("Content-Type", "application/json")
	res, err := client.Do(request)
	if err != nil {
		b.Fatalf("do request: %v", err)
	}
	defer func() {
		_, err := io.Copy(io.Discard, res.Body)
		assert.Nil(b, err)
	}()
	if res.StatusCode != http.StatusOK {
		b.Fatalf("response status: %v", res.Status)
	}
	uncompressed, err := gzip.NewReader(res.Body)
	if err != nil {
		b.Fatalf("uncompress response: %v", err)
	}
	raw, err := io.ReadAll(uncompressed)
	if err != nil {
		b.Fatalf("read response: %v", err)
	}
	var got ping
	if err := json.Unmarshal(raw, &got); err != nil {
		b.Fatalf("unmarshal: %v", err)
	}
}
