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
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write(b)
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

	_, err := duplexCall.Write([]byte("hello"))
	assert.Nil(t, err)

	go func() {
		_, err := duplexCall.Write([]byte(", world"))
		assert.Nil(t, err)
		duplexCall.CloseWrite()
	}()

	answers := make(chan string)
	go func() {
		body, err := duplexCall.request.GetBody()
		assert.Nil(t, err)
		ans, err := io.ReadAll(body)
		assert.Nil(t, err)
		answers <- string(ans)
	}()
	got := <-answers
	assert.Equal(t, got, "hello, world")
}
