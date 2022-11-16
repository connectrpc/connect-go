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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bufbuild/connect-go/internal/assert"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestAcceptEncodingOrdering(t *testing.T) {
	t.Parallel()
	const (
		compressionBrotli = "br"
		expect            = compressionGzip + "," + compressionBrotli
	)

	withFakeBrotli, ok := withGzip().(*compressionOption)
	assert.True(t, ok)
	withFakeBrotli.Name = compressionBrotli

	var called bool
	verify := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(connectUnaryHeaderAcceptCompression)
		assert.Equal(t, got, expect)
		w.WriteHeader(http.StatusOK)
		called = true
	})
	server := httptest.NewServer(verify)
	defer server.Close()

	client := NewClient[emptypb.Empty, emptypb.Empty](
		server.Client(),
		server.URL,
		withFakeBrotli,
		withGzip(),
	)
	_, _ = client.CallUnary(context.Background(), NewRequest(&emptypb.Empty{}))
	assert.True(t, called)
}

func TestFailCompression(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	pingclient := NewClient[emptypb.Empty, emptypb.Empty](
		server.Client(),
		server.URL,
		withFailCompression(),
		WithSendCompression("error"),
		WithCompressMinBytes(1),
	)
	t.Run("request_uncompressed", func(t *testing.T) {
		t.Parallel()
		_, err := pingclient.CallUnary(context.Background(), NewRequest(&emptypb.Empty{}))
		assert.NotEqual(t, err, errors.New(""))
	})
}

type failDecompressor struct {
	Decompressor
}

type failCompressor struct{}

func (failCompressor) Write([]byte) (int, error) {
	return 0, errors.New("failCompressor")
}

func (failCompressor) Close() error {
	return errors.New("failCompressor")
}

func (failCompressor) Reset(io.Writer) {}

func withFailCompression() ClientOption {
	return WithAcceptCompression(
		"error",
		func() Decompressor { return &failDecompressor{} },
		func() Compressor { return &failCompressor{} },
	)
}
