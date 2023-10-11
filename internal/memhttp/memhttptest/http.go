// Copyright 2021-2023 The Connect Authors
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

package memhttptest

import (
	"context"
	"log"
	"net/http"
	"testing"

	"connectrpc.com/connect/internal/memhttp"
)

// NewServer constructs a [memhttp.Server] with defaults suitable for tests:
// it logs runtime errors to the provided testing.TB, and it automatically shuts
// down the server when the test completes. Startup and shutdown errors fail the
// test.
//
// To customize the server, use any [memhttp.Option]. In particular, it may be
// necessary to customize the shutdown timeout with
// [memhttp.WithCleanupTimeout].
func NewServer(tb testing.TB, handler http.Handler, opts ...memhttp.Option) *memhttp.Server {
	tb.Helper()
	logger := log.New(&testWriter{tb}, "" /* prefix */, log.Lshortfile)
	opts = append([]memhttp.Option{memhttp.WithErrorLog(logger)}, opts...)
	server := memhttp.NewServer(handler, opts...)
	tb.Cleanup(func() {
		tb.Logf("shutting down server")
		if err := server.Shutdown(context.Background()); err != nil {
			tb.Error(err)
		}
	})
	return server
}

// testWriter is an io.Writer that logs to the testing.TB.
type testWriter struct {
	tb testing.TB
}

func (l *testWriter) Write(p []byte) (int, error) {
	l.tb.Log(string(p))
	return len(p), nil
}
