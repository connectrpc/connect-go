// Copyright 2021-2023 Buf Technologies, Inc.
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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/bufbuild/connect-go/internal/assert"
)

func TestDuplexHTTPCallGetBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello world"))
	}))
	t.Cleanup(server.Close)

	serverURL, _ := url.Parse(server.URL)
	duplexCall := newDuplexHTTPCall(
		context.Background(),
		server.Client(),
		serverURL,
		Spec{StreamType: StreamTypeUnary},
		http.Header{},
		newBufferPool(),
	)

	_, err := duplexCall.Write([]byte("hello world"))
	assert.Nil(t, err)
	body, err := duplexCall.request.GetBody()
	assert.Nil(t, err)
	ans, err := io.ReadAll(body)
	assert.Nil(t, err)
	assert.Equal(t, "hello world", string(ans))
	assert.Nil(t, duplexCall.CloseWrite())
}
