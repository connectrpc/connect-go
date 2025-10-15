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

//go:build go1.25

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

package memhttptest_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"connectrpc.com/connect/internal/assert"
	"connectrpc.com/connect/internal/memhttp"
	"connectrpc.com/connect/internal/memhttp/memhttptest"
)

// TestMemhttpWithSynctest verifies that memhttp works correctly with synctest.
func TestMemhttpWithSynctest(t *testing.T) {
	t.Parallel()
	body := "request body"

	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		buf := &bytes.Buffer{}
		_, err := io.Copy(buf, request.Body)
		if err != nil {
			t.Errorf("failed to copy body: %v", err)
		}
		if buf.String() != body {
			t.Errorf("got body %q, want %q", buf.String(), body)
		}
		writer.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name   string
		client func(*testing.T, *memhttp.Server) *http.Client
	}{
		{
			name: "server.Client()",
			client: func(t *testing.T, s *memhttp.Server) *http.Client {
				t.Helper()
				return s.Client()
			},
		},
		{
			name: "Custom Client HTTP/1",
			client: func(t *testing.T, s *memhttp.Server) *http.Client {
				t.Helper()
				// HTTP/1.1's is a per-request closure, so nothing leaks outside the bubble.
				return &http.Client{Transport: s.TransportHTTP1()}
			},
		},
		{
			name: "Custom Client HTTP/2",
			client: func(t *testing.T, s *memhttp.Server) *http.Client {
				t.Helper()
				// HTTP/2 a goroutine running for future connections, which leaks outside the bubble.
				client := &http.Client{Transport: s.Transport()}
				// Closing idle connections here ensures synctest doesn't panic.
				t.Cleanup(client.CloseIdleConnections)
				return client
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				t.Helper()
				server := memhttptest.NewServer(t, handler)

				req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, server.URL(), strings.NewReader(body))
				assert.Nil(t, err)

				client := test.client(t, server)
				resp, err := client.Do(req)
				assert.Nil(t, err)
				resp.Body.Close()
			})
		})
	}
}
