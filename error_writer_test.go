// Copyright 2021-2024 The Connect Authors
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
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/internal/assert"
)

func TestErrorWriter(t *testing.T) {
	t.Parallel()

	t.Run("RequireConnectProtocolHeader", func(t *testing.T) {
		t.Parallel()
		writer := NewErrorWriter(WithRequireConnectProtocolHeader())

		t.Run("Unary", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", connectUnaryContentTypePrefix+codecNameJSON)
			assert.False(t, writer.IsSupported(req))
			req.Header.Set(connectHeaderProtocolVersion, connectProtocolVersion)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("UnaryGET", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://localhost", nil)
			assert.False(t, writer.IsSupported(req))
			query := req.URL.Query()
			query.Set(connectUnaryConnectQueryParameter, connectUnaryConnectQueryValue)
			req.URL.RawQuery = query.Encode()
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("Stream", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", connectStreamingContentTypePrefix+codecNameJSON)
			assert.True(t, writer.IsSupported(req)) // ignores WithRequireConnectProtocolHeader
			req.Header.Set(connectHeaderProtocolVersion, connectProtocolVersion)
			assert.True(t, writer.IsSupported(req))
		})
	})
}
