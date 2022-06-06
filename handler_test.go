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

package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bufbuild/connect-go/internal/assert"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
)

func TestHandler_ServeHTTP(t *testing.T) {
	t.Parallel()
	const procedure = "/connect.ping.v1.PingService/Ping"
	handler := NewUnaryHandler[pingv1.PingRequest, pingv1.PingResponse](
		procedure,
		func(ctx context.Context, req *Request[pingv1.PingRequest]) (*Response[pingv1.PingResponse], error) {
			return nil, NewError(CodeUnimplemented, nil)
		})
	server := httptest.NewServer(handler)
	client := server.Client()
	t.Cleanup(func() {
		server.Close()
	})

	t.Run("method_not_allowed", func(t *testing.T) {
		t.Parallel()
		resp, err := client.Get(server.URL + procedure)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusMethodNotAllowed)
		assert.Equal(t, resp.Header.Get("Allow"), http.MethodPost)
	})

	t.Run("unsupported_content_type", func(t *testing.T) {
		t.Parallel()
		resp, err := client.Post(server.URL+procedure, "application/x-custom-json", strings.NewReader("{}"))
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusUnsupportedMediaType)
		assert.Equal(t, resp.Header.Get("Accept-Post"), handler.acceptPost)
	})
}
