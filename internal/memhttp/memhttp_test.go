// Copyright 2021-2023 The Connect Authors
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
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/internal/assert"
	"connectrpc.com/connect/internal/memhttp"
	"connectrpc.com/connect/internal/memhttp/memhttptest"
)

func TestServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		opts    []memhttp.Option
		handler func(t *testing.T, w http.ResponseWriter, r *http.Request)
	}{{
		name: "http2",
		opts: nil,
		handler: func(t *testing.T, _ http.ResponseWriter, r *http.Request) {
			t.Helper()
			assert.Equal(t, r.ProtoMajor, 2)
			assert.Equal(t, r.ProtoMinor, 0)
		},
	}, {
		name: "http1",
		opts: []memhttp.Option{memhttp.WithoutHTTP2()},
		handler: func(t *testing.T, _ http.ResponseWriter, r *http.Request) {
			t.Helper()
			assert.Equal(t, r.ProtoMajor, 1)
			assert.Equal(t, r.ProtoMinor, 1)
		},
	}}
	for _, testcase := range tests {
		testcase := testcase
		t.Run(testcase.name, func(t *testing.T) {
			t.Parallel()
			const concurrency = 100
			const greeting = "Hello, world!"

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				testcase.handler(t, w, r)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(greeting))
			})
			server := memhttptest.NewServer(t, handler, testcase.opts...)
			var wg sync.WaitGroup
			for i := 0; i < concurrency; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					client := server.Client()
					req, err := http.NewRequestWithContext(
						context.Background(),
						http.MethodGet,
						server.URL(),
						strings.NewReader(""),
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
	assert.Nil(t, server.Shutdown(context.Background()))
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
	fmt.Println(res.Status)
	// Output:
	// 200 OK
}

func ExampleServer_Shutdown() {
	hello := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "Hello, world!")
	})
	srv := memhttp.NewServer(hello)
	srv.RegisterOnShutdown(func() {
		fmt.Println("Server is shutting down")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		panic(err)
	}
	// Output:
	// Server is shutting down
}
