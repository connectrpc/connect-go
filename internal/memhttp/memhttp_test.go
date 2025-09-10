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

package memhttp_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/internal/assert"
	"connectrpc.com/connect/internal/memhttp"
	"connectrpc.com/connect/internal/memhttp/memhttptest"
)

func TestServerTransport(t *testing.T) {
	t.Parallel()
	concurrency := runtime.GOMAXPROCS(0) * 8
	const greeting = "Hello, world!"

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(greeting))
	})
	server := memhttptest.NewServer(t, handler)

	for _, transport := range []http.RoundTripper{
		server.Transport(),
		server.TransportHTTP1(),
	} {
		client := &http.Client{Transport: transport}
		t.Run(fmt.Sprintf("%T", transport), func(t *testing.T) {
			t.Parallel()
			var wg sync.WaitGroup
			for range concurrency {
				wg.Add(1)
				go func() {
					defer wg.Done()
					req, err := http.NewRequestWithContext(
						t.Context(),
						http.MethodGet,
						server.URL(),
						nil,
					)
					assert.Nil(t, err)
					res, err := client.Do(req)
					assert.Nil(t, err)
					assert.Equal(t, res.StatusCode, http.StatusOK)
					body, err := io.ReadAll(res.Body)
					assert.Nil(t, err)
					assert.Nil(t, res.Body.Close())
					assert.Equal(t, string(body), greeting)
				}()
			}
			wg.Wait()
		})
	}
}

func TestRegisterOnShutdown(t *testing.T) {
	t.Parallel()
	okay := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	server := memhttp.NewServer(okay)
	done := make(chan struct{})
	server.RegisterOnShutdown(func() {
		close(done)
	})
	assert.Nil(t, server.Shutdown(t.Context()))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("OnShutdown hook didn't fire")
	}
}

func Example() {
	hello := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "Hello, world!")
	})
	srv := memhttp.NewServer(hello)
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL())
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()
	fmt.Println(res.Status)
	// Output:
	// 200 OK
}

func ExampleServer_Client() {
	hello := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "Hello, world!")
	})
	srv := memhttp.NewServer(hello)
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 10 * time.Second
	res, err := client.Get(srv.URL())
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()
	fmt.Println(res.Status)
	// Output:
	// 200 OK
}

func ExampleServer_Shutdown() {
	hello := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "Hello, world!")
	})
	srv := memhttp.NewServer(hello)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		panic(err)
	}
	fmt.Println("Server has shut down")
	// Output:
	// Server has shut down
}
