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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/internal/assert"
	"connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect"
)

func TestHandler_ServeHTTP(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(
		pingServer{},
	))
	server := httptest.NewServer(mux)
	client := server.Client()
	t.Cleanup(func() {
		server.Close()
	})

	t.Run("method_not_allowed", func(t *testing.T) {
		t.Parallel()
		resp, err := client.Get(server.URL + "/" + pingv1connect.PingServiceName + "/Ping")
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusMethodNotAllowed)
		assert.Equal(t, resp.Header.Get("Allow"), http.MethodPost)
	})

	t.Run("unsupported_content_type", func(t *testing.T) {
		t.Parallel()
		resp, err := client.Post(server.URL+"/"+pingv1connect.PingServiceName+"/Ping", "application/x-custom-json", strings.NewReader("{}"))
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusUnsupportedMediaType)
		assert.Equal(t, resp.Header.Get("Accept-Post"), strings.Join([]string{
			"application/grpc",
			"application/grpc+json",
			"application/grpc+proto",
			"application/grpc-web",
			"application/grpc-web+json",
			"application/grpc-web+proto",
			"application/json",
			"application/proto",
		}, ", "))
	})
}
