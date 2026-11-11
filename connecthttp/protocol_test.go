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
	"io"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
)

func TestCanonicalizeContentType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		arg  string
		want string
	}{
		{name: "uppercase should be normalized", arg: "APPLICATION/json", want: "application/json"},
		{name: "utf-8 charset param should be stripped", arg: "application/json; charset=UTF-8", want: "application/json"},
		{name: "non-utf-8 charset param should be lowercased", arg: "application/json; charset=Shift-JIS", want: "application/json; charset=shift-jis"},
		{name: "no parameters should be normalized", arg: "APPLICATION/json;  ", want: "application/json"},
		{name: "non charset param should be dropped", arg: "application/proto; foo=bar", want: "application/proto"},
		{name: "non charset params should be dropped from json", arg: "application/json; foo=bar; Baz=qux", want: "application/json"},
		{name: "non charset param should be dropped alongside charset", arg: "application/json; foo=bar; charset=latin1", want: "application/json; charset=latin1"},
		{name: "middle charset should be kept on json", arg: "application/json; a=1; charset=latin1; b=2", want: "application/json; charset=latin1"},
		{name: "middle utf-8 charset should be stripped", arg: "application/json; a=1; Charset=UTF-8; b=2", want: "application/json"},
		{name: "last charset should be kept on streaming json", arg: "application/connect+json; a=1; b=2; charset=latin1", want: "application/connect+json; charset=latin1"},
		{name: "last charset should be ignored on proto", arg: "application/proto; a=1; charset=latin1", want: "application/proto"},
		{name: "unrelated media type params should be dropped", arg: "multipart/form-data; boundary=fooBar", want: "multipart/form-data"},
		{name: "charset without space should be stripped", arg: "application/json;charset=utf-8", want: "application/json"},
		{name: "uppercase charset key should be stripped", arg: "Application/JSON; Charset=UTF-8", want: "application/json"},
		{name: "quoted utf-8 charset should be stripped", arg: `application/json; charset="utf-8"`, want: "application/json"},
		{name: "empty charset should be kept on json", arg: `application/json; charset=""`, want: `application/json; charset=""`},
		{name: "charset should be ignored on proto", arg: "application/proto; charset=latin1", want: "application/proto"},
		{name: "charset should be ignored on streaming proto", arg: "application/connect+proto; charset=latin1", want: "application/connect+proto"},
		{name: "utf-8 charset should be stripped from streaming json", arg: "application/connect+json; charset=utf-8", want: "application/connect+json"},
		{name: "non-utf-8 charset should be kept on streaming json", arg: "application/connect+json; charset=Latin1", want: "application/connect+json; charset=latin1"},
		{name: "non-utf-8 charset should be kept on grpc json", arg: "application/grpc+json; charset=latin1", want: "application/grpc+json; charset=latin1"},
		{name: "charset should be ignored on custom codec", arg: "application/json-custom; charset=latin1", want: "application/json-custom"},
		{name: "uppercase streaming type should be normalized", arg: "Application/Connect+PROTO", want: "application/connect+proto"},
		{name: "malformed parameter should be unchanged", arg: "application/proto; foo", want: "application/proto; foo"},
		{name: "duplicate parameter should be unchanged", arg: "application/proto; foo=a; foo=b", want: "application/proto; foo=a; foo=b"},
		{name: "missing plus should not be fixed", arg: "application/connectproto", want: "application/connectproto"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, canonicalizeContentType(tt.arg), tt.want)
		})
	}
}

func BenchmarkCanonicalizeContentType(b *testing.B) {
	b.Run("simple", func(b *testing.B) {
		for b.Loop() {
			_ = canonicalizeContentType("application/json")
		}
		b.ReportAllocs()
	})

	b.Run("with charset", func(b *testing.B) {
		for b.Loop() {
			_ = canonicalizeContentType("application/json; charset=utf-8")
		}
		b.ReportAllocs()
	})

	b.Run("with other param", func(b *testing.B) {
		for b.Loop() {
			_ = canonicalizeContentType("application/json; foo=utf-8")
		}
		b.ReportAllocs()
	})
}

func TestNegotiateCompression(t *testing.T) {
	t.Parallel()
	// pools builds the server's compressors. Order is the order they're
	// advertised in, but the client's Accept-Encoding order decides the
	// response encoding.
	pools := func(registered ...string) readOnlyCompressionPools {
		compressors := make([]connect.Compressor, 0, len(registered))
		for _, name := range registered {
			compressors = append(compressors, stubCompressor(name))
		}
		return newReadOnlyCompressionPools(compressors)
	}

	tests := []struct {
		name         string
		pools        readOnlyCompressionPools
		sent         string
		accept       string
		wantRequest  string
		wantResponse string
		wantErrCode  connect.Code
	}{{
		name:         "server supports gzip only",
		pools:        pools("gzip"),
		accept:       "gzip,br,zstd",
		wantRequest:  "identity",
		wantResponse: "gzip",
	}, {
		name:         "client order wins",
		pools:        pools("gzip", "zstd", "br"),
		accept:       "br,gzip,zstd",
		wantRequest:  "identity",
		wantResponse: "br",
	}, {
		name:         "client order wins over server order",
		pools:        pools("br", "gzip"),
		accept:       "gzip,br",
		wantRequest:  "identity",
		wantResponse: "gzip",
	}, {
		name:         "skips unsupported client preferences",
		pools:        pools("gzip"),
		accept:       "zstd,br,gzip",
		wantRequest:  "identity",
		wantResponse: "gzip",
	}, {
		// Compression is symmetric: a compressed request is answered in the
		// same encoding, whatever the client accepts.
		name:         "request encoding echoed",
		pools:        pools("br", "gzip"),
		sent:         "gzip",
		accept:       "br,gzip",
		wantRequest:  "gzip",
		wantResponse: "gzip",
	}, {
		name:         "request encoding echoed without accept",
		pools:        pools("br", "gzip"),
		sent:         "gzip",
		wantRequest:  "gzip",
		wantResponse: "gzip",
	}, {
		name:         "explicit identity request encoding",
		pools:        pools("br", "gzip"),
		sent:         "identity",
		accept:       "gzip,br",
		wantRequest:  "identity",
		wantResponse: "gzip",
	}, {
		name:         "no mutually supported encoding",
		pools:        pools("gzip"),
		accept:       "br,zstd",
		wantRequest:  "identity",
		wantResponse: "identity",
	}, {
		name:         "identity accepted",
		pools:        pools("gzip"),
		accept:       "identity",
		wantRequest:  "identity",
		wantResponse: "identity",
	}, {
		name:         "space separated accept",
		pools:        pools("br", "gzip"),
		accept:       "gzip, br",
		wantRequest:  "identity",
		wantResponse: "gzip",
	}, {
		name:         "no compressors registered",
		pools:        pools(),
		accept:       "gzip,br",
		wantRequest:  "identity",
		wantResponse: "identity",
	}, {
		name:        "unknown request encoding",
		pools:       pools("gzip"),
		sent:        "br",
		accept:      "gzip",
		wantErrCode: connect.CodeUnimplemented,
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request, response, err := negotiateCompression(test.pools, test.sent, test.accept)
			if test.wantErrCode != 0 {
				assert.NotNil(t, err)
				assert.Equal(t, connect.CodeOf(err), test.wantErrCode)
				return
			}
			assert.Nil(t, err)
			assert.Equal(t, request, test.wantRequest)
			assert.Equal(t, response, test.wantResponse)
		})
	}
}

func BenchmarkNegotiateCompression(b *testing.B) {
	pools := func(registered ...string) readOnlyCompressionPools {
		compressors := make([]connect.Compressor, 0, len(registered))
		for _, name := range registered {
			compressors = append(compressors, stubCompressor(name))
		}
		return newReadOnlyCompressionPools(compressors)
	}

	b.Run("default", func(b *testing.B) {
		available := pools(connect.CompressionNameGzip)
		for b.Loop() {
			_, _, _ = negotiateCompression(available, "", "gzip")
		}
		b.ReportAllocs()
	})

	b.Run("multiple compressors", func(b *testing.B) {
		available := pools(connect.CompressionNameGzip, "zstd", "br")
		for b.Loop() {
			_, _, _ = negotiateCompression(available, "", "gzip,br,zstd")
		}
		b.ReportAllocs()
	})

	b.Run("no mutually supported encoding", func(b *testing.B) {
		available := pools(connect.CompressionNameGzip)
		for b.Loop() {
			_, _, _ = negotiateCompression(available, "", "br,zstd")
		}
		b.ReportAllocs()
	})
}

// stubCompressor is a [connect.Compressor] that only carries a name. The
// negotiation tests exercise name selection, never the payload path.
type stubCompressor string

func (c stubCompressor) Name() string { return string(c) }

func (stubCompressor) Compress(dst io.Writer) (io.WriteCloser, error) {
	return identityWriteCloser{dst}, nil
}

func (stubCompressor) Decompress(src io.Reader) (io.ReadCloser, error) {
	return io.NopCloser(src), nil
}
