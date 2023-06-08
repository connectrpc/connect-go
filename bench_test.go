// Copyright 2021-2023 Buf Technologies, Inc.
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

	connect "github.com/bufbuild/connect-go"
	"github.com/bufbuild/connect-go/internal/assert"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
	"github.com/bufbuild/connect-go/internal/gen/connect/ping/v1/pingv1connect"
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

	client := pingv1connect.NewPingServiceClient(
		httpClient,
		server.URL,
		connect.WithGRPC(),
		connect.WithSendGzip(),
	)
	twoMiB := strings.Repeat("a", 2*1024*1024)
	b.ResetTimer()

	b.Run("unary", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = client.Ping(
					context.Background(),
					connect.NewRequest(&pingv1.PingRequest{Text: twoMiB}),
				)
			}
		})
	})
}

type ping struct {
	Text string `json:"text"`
}

func BenchmarkREST(b *testing.B) {
	handler := func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		defer func() {
			_, err := io.Copy(io.Discard, request.Body)
			assert.Nil(b, err)
		}()
		writer.Header().Set("Content-Type", "application/json")
		var body io.Reader = request.Body
		if request.Header.Get("Content-Encoding") == "gzip" {
			gzipReader, err := gzip.NewReader(body)
			if err != nil {
				b.Fatalf("get gzip reader: %v", err)
			}
			defer gzipReader.Close()
			body = gzipReader
		}
		var out io.Writer = writer
		if strings.Contains(request.Header.Get("Accept-Encoding"), "gzip") {
			writer.Header().Set("Content-Encoding", "gzip")
			gzipWriter := gzip.NewWriter(writer)
			defer gzipWriter.Close()
			out = gzipWriter
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			b.Fatalf("read body: %v", err)
		}
		var pingRequest ping
		if err := json.Unmarshal(raw, &pingRequest); err != nil {
			b.Fatalf("json unmarshal: %v", err)
		}
		bs, err := json.Marshal(&pingRequest)
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
	twoMiB := strings.Repeat("a", 2*1024*1024)
	b.ResetTimer()

	b.Run("unary", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				unaryRESTIteration(b, server.Client(), server.URL, twoMiB)
			}
		})
	})
}

func unaryRESTIteration(b *testing.B, client *http.Client, url string, text string) {
	b.Helper()
	rawRequestBody := bytes.NewBuffer(nil)
	compressedRequestBody := gzip.NewWriter(rawRequestBody)
	encoder := json.NewEncoder(compressedRequestBody)
	if err := encoder.Encode(&ping{text}); err != nil {
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
	response, err := client.Do(request)
	if err != nil {
		b.Fatalf("do request: %v", err)
	}
	defer func() {
		_, err := io.Copy(io.Discard, response.Body)
		assert.Nil(b, err)
	}()
	if response.StatusCode != http.StatusOK {
		b.Fatalf("response status: %v", response.Status)
	}
	uncompressed, err := gzip.NewReader(response.Body)
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

type noopResponseWriter struct {
	header http.Header
}

func (w noopResponseWriter) Header() http.Header         { return w.header }
func (w noopResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w noopResponseWriter) WriteHeader(statusCode int)  {}

func BenchmarkMux(b *testing.B) {
	resp := []byte("OK")
	fn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(resp)
	})

	b.Run("net/http", func(b *testing.B) {
		mux := http.NewServeMux()
		mux.Handle("/example.v1.Service/A", fn)
		mux.Handle("/example.v1.Service/B", fn)
		mux.Handle("/example.v1.Service/C", fn)
		mux.Handle("/example.v1.Service/D", fn)
		mux.Handle("/example.v1.Service/E", fn)

		w := noopResponseWriter{make(http.Header, 100)}
		req, _ := http.NewRequest("POST", "http://localhost/example.v1.Service/E", nil)

		b.StartTimer()
		for i := 0; i < b.N; i++ {
			mux.ServeHTTP(w, req)
		}
		b.StopTimer()
	})

	b.Run("switch", func(b *testing.B) {
		mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/example.v1.Service/A":
				fn.ServeHTTP(w, r)
			case "/example.v1.Service/B":
				fn.ServeHTTP(w, r)
			case "/example.v1.Service/C":
				fn.ServeHTTP(w, r)
			case "/example.v1.Service/D":
				fn.ServeHTTP(w, r)
			case "/example.v1.Service/E":
				fn.ServeHTTP(w, r)
			default:
				http.NotFound(w, r)
			}
		})
		w := noopResponseWriter{make(http.Header, 100)}
		req, _ := http.NewRequest("POST", "http://localhost/example.v1.Service/E", nil)

		b.StartTimer()
		for i := 0; i < b.N; i++ {
			mux.ServeHTTP(w, req)
		}
		b.StopTimer()
	})
}
