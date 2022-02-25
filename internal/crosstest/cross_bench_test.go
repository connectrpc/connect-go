package crosstest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	grpcgzip "google.golang.org/grpc/encoding/gzip"

	"github.com/bufbuild/connect"
	connectgzip "github.com/bufbuild/connect/compress/gzip"
	"github.com/bufbuild/connect/internal/assert"
	crossrpc "github.com/bufbuild/connect/internal/crosstest/gen/proto/go-connect/cross/v1test"
	crosspb "github.com/bufbuild/connect/internal/crosstest/gen/proto/go/cross/v1test"
)

func BenchmarkConnect(b *testing.B) {
	mux := http.NewServeMux()
	mux.Handle(crossrpc.NewCrossServiceHandler(crossServerConnect{}))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	doer := server.Client()
	httpTransport, ok := doer.Transport.(*http.Transport)
	assert.True(b, ok, "expected HTTP client to have *http.Transport as RoundTripper")
	httpTransport.DisableCompression = true

	client, err := crossrpc.NewCrossServiceClient(
		server.URL,
		server.Client(),
		connect.WithRequestCompressor(connectgzip.Name),
	)
	assert.Nil(b, err, "client construction error")
	b.ResetTimer()

	b.Run("unary", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = client.Ping(
					context.Background(),
					connect.NewEnvelope(&crosspb.PingRequest{Number: 42}),
				)
			}
		})
	})
}

func BenchmarkREST(b *testing.B) {
	type ping struct {
		Number int `json:"number"`
	}

	handler := func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		defer io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gr, err := gzip.NewReader(body)
			if err != nil {
				b.Fatalf("get gzip reader: %v", err)
			}
			defer gr.Close()
			body = gr
		}
		var out io.Writer = w
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gw := gzip.NewWriter(w)
			defer gw.Close()
			out = gw
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			b.Fatalf("read body: %v", err)
		}
		var req ping
		if err := json.Unmarshal(raw, &req); err != nil {
			b.Fatalf("json unmarshal: %v", err)
		}
		bs, err := json.Marshal(&req)
		if err != nil {
			b.Fatalf("json marshal: %v", err)
		}
		out.Write(bs)
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(handler))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	b.ResetTimer()

	b.Run("unary", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
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
					server.URL,
					rawRequestBody,
				)
				if err != nil {
					b.Fatalf("construct request: %v", err)
				}
				request.Header.Set("Content-Encoding", "gzip")
				request.Header.Set("Accept-Encoding", "gzip")
				request.Header.Set("Content-Type", "application/json")
				res, err := server.Client().Do(request)
				if err != nil {
					b.Fatalf("do request: %v", err)
				}
				defer io.Copy(io.Discard, res.Body)
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
		})
	})
}

func BenchmarkGRPC(b *testing.B) {
	lis, err := net.Listen("tcp", "localhost:0")
	assert.Nil(b, err, "listen on ephemeral port")
	server := grpc.NewServer()
	crosspb.RegisterCrossServiceServer(server, crossServerGRPC{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(lis)
	}()
	defer wg.Wait()
	defer server.GracefulStop()

	gconn, err := grpc.Dial(
		lis.Addr().String(),
		grpc.WithInsecure(),
	)
	assert.Nil(b, err, "grpc dial")
	client := crosspb.NewCrossServiceClient(gconn)

	b.ResetTimer()

	b.Run("unary", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = client.Ping(context.Background(), &crosspb.PingRequest{Number: 42}, grpc.UseCompressor(grpcgzip.Name))
			}
		})
	})
}
