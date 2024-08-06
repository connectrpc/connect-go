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

func TestCanonicalizeContentType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		arg  string
		want string
	}{
		{name: "uppercase should be normalized", arg: "APPLICATION/json", want: "application/json"},
		{name: "charset param should be treated as lowercase", arg: "application/json; charset=UTF-8", want: "application/json; charset=utf-8"},
		{name: "non charset param should not be changed", arg: "multipart/form-data; boundary=fooBar", want: "multipart/form-data; boundary=fooBar"},
		{name: "no parameters should be normalized", arg: "APPLICATION/json;  ", want: "application/json"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, canonicalizeContentType(tt.arg), tt.want)
		})
	}
}

func BenchmarkCanonicalizeContentType(b *testing.B) {
	b.Run("simple", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = canonicalizeContentType("application/json")
		}
		b.ReportAllocs()
	})

	b.Run("with charset", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = canonicalizeContentType("application/json; charset=utf-8")
		}
		b.ReportAllocs()
	})

	b.Run("with other param", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = canonicalizeContentType("application/json; foo=utf-8")
		}
		b.ReportAllocs()
	})
}

func TestProtocolFromRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		contentType string
		method      string
		want        string
		valid       bool
	}{{
		name:        "connectUnary",
		contentType: "application/json",
		method:      http.MethodPost,
		want:        ProtocolConnect,
		valid:       true,
	}, {
		name:        "connectStreaming",
		contentType: "application/connec+json",
		method:      http.MethodPost,
		want:        ProtocolConnect,
		valid:       true,
	}, {
		name:        "grpcWeb",
		contentType: "application/grpc-web",
		method:      http.MethodPost,
		want:        ProtocolGRPCWeb,
		valid:       true,
	}, {
		name:        "grpc",
		contentType: "application/grpc",
		method:      http.MethodPost,
		want:        ProtocolGRPC,
		valid:       true,
	}, {
		name:        "connectGet",
		contentType: "application/connec+json",
		method:      http.MethodGet,
		want:        ProtocolConnect,
		valid:       true,
	}, {
		name:        "grpcWebGet",
		contentType: "application/grpc-web",
		method:      http.MethodGet,
		want:        ProtocolConnect,
		valid:       true,
	}, {
		name:        "grpcGet",
		contentType: "application/grpc+json",
		method:      http.MethodGet,
		want:        ProtocolConnect,
		valid:       true,
	}, {
		name:        "unknown",
		contentType: "text/html",
		method:      http.MethodPost,
		valid:       false,
	}}
	for _, testcase := range tests {
		testcase := testcase
		t.Run(testcase.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(testcase.method, "http://localhost:8080/service/Method", nil)
			if testcase.contentType != "" {
				req.Header.Set("Content-Type", testcase.contentType)
			}
			req.Method = testcase.method
			got, valid := ProtocolFromRequest(req)
			assert.Equal(t, got, testcase.want, assert.Sprintf("protocol"))
			assert.Equal(t, valid, testcase.valid, assert.Sprintf("valid"))
		})
	}
}

func TestProcedureFromURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		url   string
		want  string
		valid bool
	}{
		{name: "simple", url: "http://localhost:8080/foo", want: "/foo"},
		{name: "service", url: "http://localhost:8080/service/bar", want: "/service/bar", valid: true},
		{name: "trailing", url: "http://localhost:8080/service/bar/", want: "/service/bar", valid: true},
		{name: "subroute", url: "http://localhost:8080/api/service/bar/", want: "/service/bar", valid: true},
		{name: "subrouteTrailing", url: "http://localhost:8080/api/service/bar/", want: "/service/bar", valid: true},
		{name: "missingService", url: "http://localhost:8080//foo", want: "//foo"},
		{name: "missingMethod", url: "http://localhost:8080/foo//", want: "/foo//"},
		{
			name:  "real",
			url:   "http://localhost:8080/connect.ping.v1.PingService/Ping",
			want:  "/connect.ping.v1.PingService/Ping",
			valid: true,
		},
	}
	for _, testcase := range tests {
		testcase := testcase
		t.Run(testcase.name, func(t *testing.T) {
			t.Parallel()
			url, err := url.Parse(testcase.url)
			if !assert.Nil(t, err) {
				return
			}
			t.Log(url.String())
			got, valid := ProcedureFromURL(url)
			assert.Equal(t, got, testcase.want)
			assert.Equal(t, valid, testcase.valid)
		})
	}
}
