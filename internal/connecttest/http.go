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

package connecttest

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// StartHTTPTestServer starts an HTTP server that listens on a in memory
// network. The returned server is configured to use the in memory network.
func StartHTTPTestServer(tb testing.TB, handler http.Handler) *httptest.Server {
	tb.Helper()
	lis := NewMemoryListener()
	svr := httptest.NewUnstartedServer(handler)
	svr.Config.ErrorLog = log.New(NewTestWriter(tb), "", 0) //nolint:forbidigo
	svr.Listener = lis
	svr.Start()
	tb.Cleanup(svr.Close)
	client := svr.Client()
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.DialContext = lis.DialContext
	} else {
		tb.Fatalf("unexpected transport type: %T", client.Transport)
	}
	return svr
}

// StartHTTP2TestServer starts an HTTP/2 server that listens on a in memory
// network. The returned server is configured to use the in memory network and
// TLS.
func StartHTTP2TestServer(tb testing.TB, handler http.Handler) *httptest.Server {
	tb.Helper()
	lis := NewMemoryListener()
	svr := httptest.NewUnstartedServer(handler)
	svr.Config.ErrorLog = log.New(NewTestWriter(tb), "", 0) //nolint:forbidigo
	svr.Listener = lis
	svr.EnableHTTP2 = true
	svr.StartTLS()
	tb.Cleanup(svr.Close)
	client := svr.Client()
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.DialContext = lis.DialContext
	} else {
		tb.Fatalf("unexpected transport type: %T", client.Transport)
	}
	return svr
}

type testWriter struct {
	tb testing.TB
}

func (l *testWriter) Write(p []byte) (int, error) {
	l.tb.Log(string(p))
	return len(p), nil
}

// NewTestWriter returns a writer that logs to the given testing.TB.
func NewTestWriter(tb testing.TB) io.Writer {
	tb.Helper()
	return &testWriter{tb}
}

// MemoryListener is a net.Listener that listens on an in memory network.
type MemoryListener struct {
	conns  chan chan net.Conn
	once   sync.Once
	closed chan struct{}
}

func NewMemoryListener() *MemoryListener {
	return &MemoryListener{
		conns:  make(chan chan net.Conn),
		closed: make(chan struct{}),
	}
}

// Accept implements net.Listener.
func (l *MemoryListener) Accept() (net.Conn, error) {
	aerr := func(err error) error {
		return &net.OpError{
			Op:   "accept",
			Net:  memoryAddr{}.Network(),
			Addr: memoryAddr{},
			Err:  err,
		}
	}
	select {
	case <-l.closed:
		return nil, aerr(errors.New("listener closed"))
	case accept := <-l.conns:
		server, client := net.Pipe()
		accept <- client
		return server, nil
	}
}

// Close implements net.Listener.
func (l *MemoryListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
	})
	return nil
}

// Addr implements net.Listener.
func (l *MemoryListener) Addr() net.Addr {
	return &memoryAddr{}
}

// DialContext is the type expected by http.Transport.DialContext.
func (l *MemoryListener) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	derr := func(err error) error {
		return &net.OpError{
			Op:  "dial",
			Net: memoryAddr{}.Network(),
			Err: err,
		}
	}

	accepted := make(chan net.Conn)
	select {
	case <-ctx.Done():
		return nil, derr(ctx.Err())
	case l.conns <- accepted:
		return <-accepted, nil
	case <-l.closed:
		return nil, derr(errors.New("listener closed"))
	}
}

type memoryAddr struct{}

// Network implements net.Addr.
func (memoryAddr) Network() string { return "memory" }

// String implements io.Stringer, returning a value that matches the
// certificates used by net/http/httptest.
func (memoryAddr) String() string { return "example.com" }
