// Copyright 2021-2026 The Connect Authors
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

package connecthttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
)

func TestErrorWriter(t *testing.T) {
	t.Parallel()
	t.Run("RequireConnectProtocolHeader", func(t *testing.T) {
		t.Parallel()
		writer := NewErrorWriter(WithRequireConnectProtocolHeader())
		t.Run("Unary", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", connectUnaryContentTypePrefix+connect.CodecNameJSON)
			assert.False(t, writer.IsSupported(req))
			req.Header.Set(connectHeaderProtocolVersion, connectProtocolVersion)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("UnaryGET", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost", nil)
			assert.False(t, writer.IsSupported(req))
			query := req.URL.Query()
			query.Set(connectUnaryConnectQueryParameter, connectUnaryConnectQueryValue)
			req.URL.RawQuery = query.Encode()
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("Stream", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", connectStreamingContentTypePrefix+connect.CodecNameJSON)
			assert.True(t, writer.IsSupported(req)) // ignores WithRequireConnectProtocolHeader
			req.Header.Set(connectHeaderProtocolVersion, connectProtocolVersion)
			assert.True(t, writer.IsSupported(req))
		})
	})
	t.Run("Protocols", func(t *testing.T) {
		t.Parallel()
		writer := NewErrorWriter() // All supported by default
		t.Run("ConnectUnary", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", connectUnaryContentTypePrefix+connect.CodecNameJSON)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("ConnectUnaryGET", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost", nil)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("ConnectStream", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", connectStreamingContentTypePrefix+connect.CodecNameJSON)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("GRPC", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", grpcContentTypeDefault)
			assert.True(t, writer.IsSupported(req))
			req.Header.Set("Content-Type", grpcContentTypePrefix+"json")
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("GRPCWeb", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", grpcWebContentTypeDefault)
			assert.True(t, writer.IsSupported(req))
			req.Header.Set("Content-Type", grpcWebContentTypePrefix+"json")
			assert.True(t, writer.IsSupported(req))
		})
	})
	t.Run("UnknownCodec", func(t *testing.T) {
		t.Parallel()
		// An Unknown codec should return supported as the protocol is known and
		// the error codec is agnostic to the codec used. The server can respond
		// with a protocol error for the unknown codec.
		writer := NewErrorWriter()
		unknownCodec := "invalid"
		t.Run("ConnectUnary", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", connectUnaryContentTypePrefix+unknownCodec)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("ConnectStream", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", connectStreamingContentTypePrefix+unknownCodec)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("GRPC", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", grpcContentTypePrefix+unknownCodec)
			assert.True(t, writer.IsSupported(req))
		})
		t.Run("GRPCWeb", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost", nil)
			req.Header.Set("Content-Type", grpcWebContentTypePrefix+unknownCodec)
			assert.True(t, writer.IsSupported(req))
		})
	})
	t.Run("Scrub", func(t *testing.T) {
		t.Parallel()
		writer := NewErrorWriter()
		testCases := []struct {
			name        string
			err         error
			wantCode    connect.Code
			wantMessage string
		}{
			{name: "local", err: connect.NewError(connect.CodePermissionDenied, "no"), wantCode: connect.CodePermissionDenied, wantMessage: "no"},
			{name: "plain", err: errors.New("db password=hunter2"), wantCode: connect.CodeUnknown},
			{name: "remote", err: connect.NewError(connect.CodePermissionDenied, "downstream").WithRemote(), wantCode: connect.CodeInternal},
		}
		for _, testCase := range testCases {
			t.Run(testCase.name, func(t *testing.T) {
				t.Parallel()
				t.Run("ConnectUnary", func(t *testing.T) {
					t.Parallel()
					req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost", nil)
					req.Header.Set("Content-Type", connectUnaryContentTypePrefix+connect.CodecNameJSON)
					rec := httptest.NewRecorder()
					assert.Nil(t, writer.Write(rec, req, testCase.err))
					assert.Equal(t, rec.Code, connectCodeToHTTP(testCase.wantCode))
					var wireErr connectWireError
					assert.Nil(t, json.Unmarshal(rec.Body.Bytes(), &wireErr))
					assert.Equal(t, wireErr.Code, testCase.wantCode)
					assert.Equal(t, wireErr.Message, testCase.wantMessage)
				})
				t.Run("GRPC", func(t *testing.T) {
					t.Parallel()
					req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost", nil)
					req.Header.Set("Content-Type", grpcContentTypeDefault)
					rec := httptest.NewRecorder()
					assert.Nil(t, writer.Write(rec, req, testCase.err))
					assert.Equal(t, rec.Header().Get(grpcHeaderStatus), strconv.Itoa(int(testCase.wantCode)))
					assert.Equal(t, rec.Header().Get(grpcHeaderMessage), testCase.wantMessage)
				})
			})
		}
	})
}
