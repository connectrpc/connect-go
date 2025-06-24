// Copyright 2021-2025 The Connect Authors
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

	connect "connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/wrapped/connect/ping/v1/pingv1connect"
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
	b.Cleanup(server.Close)

	httpClient := server.Client()
	httpTransport, ok := httpClient.Transport.(*http.Transport)
	assert.True(b, ok)
	httpTransport.DisableCompression = true

	clients := []struct {
		name string
		opts []connect.ClientOption
	}{{
		name: "connect",
		opts: []connect.ClientOption{},
	}, {
		name: "grpc",
		opts: []connect.ClientOption{
			connect.WithGRPC(),
		},
	}, {
		name: "grpcweb",
		opts: []connect.ClientOption{
			connect.WithGRPCWeb(),
		},
	}}

	twoMiB := strings.Repeat("a", 2*1024*1024)
	for _, client := range clients {
		b.Run(client.name, func(b *testing.B) {
			client := pingv1connect.NewPingServiceClient(
				httpClient,
				server.URL,
				connect.WithSendGzip(),
				connect.WithClientOptions(client.opts...),
			)

			ctx := context.Background()
			b.Run("unary_big", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						if _, err := client.Ping(
							ctx, &pingv1.PingRequest{Text: twoMiB},
						); err != nil {
							b.Error(err)
						}
					}
				})
			})
			b.Run("unary_small", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						response, err := client.Ping(
							ctx, &pingv1.PingRequest{Number: 42},
						)
						if err != nil {
							b.Error(err)
						} else if num := response.GetNumber(); num != 42 {
							b.Errorf("expected 42, got %d", num)
						}
					}
				})
			})
			b.Run("client_stream", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						const (
							upTo   = 1
							expect = 1
						)
						stream := client.Sum(ctx)
						for number := int64(1); number <= upTo; number++ {
							if err := stream.Send(&pingv1.SumRequest{Number: number}); err != nil {
								b.Error(err)
							}
						}
						response, err := stream.CloseAndReceive()
						if err != nil {
							b.Error(err)
						} else if got := response.Msg.GetSum(); got != expect {
							b.Errorf("expected %d, got %d", expect, got)
						}
					}
				})
			})
			b.Run("server_stream", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						const (
							upTo = 1
						)
						request := &pingv1.CountUpRequest{Number: upTo}
						stream, err := client.CountUp(ctx, request)
						if err != nil {
							b.Error(err)
							return
						}
						number := int64(1)
						for ; stream.Receive(); number++ {
							if got := stream.Msg().GetNumber(); got != number {
								b.Errorf("expected %d, got %d", number, got)
							}
						}
						if number != upTo+1 {
							b.Errorf("expected %d, got %d", upTo+1, number)
						}
					}
				})
			})
			b.Run("bidi_stream", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						const (
							upTo = 1
						)
						stream := client.CumSum(ctx)
						number := int64(1)
						for ; number <= upTo; number++ {
							if err := stream.Send(&pingv1.CumSumRequest{Number: number}); err != nil {
								b.Error(err)
							}

							msg, err := stream.Receive()
							if err != nil {
								b.Error(err)
							}
							if got, expected := msg.GetSum(), number*(number+1)/2; got != expected {
								b.Errorf("expected %d, got %d", expected, got)
							}
						}
						if err := stream.CloseRequest(); err != nil {
							b.Error(err)
						}
						if err := stream.CloseResponse(); err != nil {
							b.Error(err)
						}
					}
				})
			})
		})
	}
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
	b.Cleanup(server.Close)
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
