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
	"net/url"
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
			req.URL.RawQuery = url.Values{
				connectUnaryConnectQueryParameter:  []string{connectUnaryConnectQueryValue},
				connectUnaryEncodingQueryParameter: []string{"json"},
				connectUnaryMessageQueryParameter:  []string{"{}"},
			}.Encode()
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
	t.Run("Protocols", func(t *testing.T) {
		t.Parallel()
		writer := NewErrorWriter() // All supported by default
		t.Run("ConnectUnary", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", connectUnaryContentTypePrefix+codecNameJSON)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("ConnectUnaryGET", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://localhost", nil)
			req.URL.RawQuery = url.Values{
				connectUnaryEncodingQueryParameter: []string{"json"},
				connectUnaryMessageQueryParameter:  []string{"{}"},
			}.Encode()
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("ConnectStream", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", connectStreamingContentTypePrefix+codecNameJSON)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("GRPC", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", grpcContentTypeDefault)
			assert.True(t, writer.IsSupported(req))
			req.Header.Set("Content-Type", grpcContentTypePrefix+"json")
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("GRPCWeb", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", grpcWebContentTypeDefault)
			assert.True(t, writer.IsSupported(req))
			req.Header.Set("Content-Type", grpcWebContentTypePrefix+"json")
			assert.True(t, writer.IsSupported(req))
		})
	})
	t.Run("UnknownCodec", func(t *testing.T) {
		// An Unknown codec should return supported as the protocol is known and
		// the error codec is agnostic to the codec used. The server can respond
		// with a protocol error for the unknown codec.
		t.Parallel()
		writer := NewErrorWriter()
		unknownCodec := "invalid"
		t.Run("ConnectUnary", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", connectUnaryContentTypePrefix+unknownCodec)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("ConnectStream", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", connectStreamingContentTypePrefix+unknownCodec)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("GRPC", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", grpcContentTypePrefix+unknownCodec)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("GRPCWeb", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", grpcWebContentTypePrefix+unknownCodec)
			assert.True(t, writer.IsSupported(req))
		})
	})
}
