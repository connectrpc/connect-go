// Copyright 2021-2025 The Connect Authors
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

package memhttp

import (
	"context"
	"errors"
	"net"
	"sync"
)

var errListenerClosed = errors.New("listener closed")

// memoryListener is a net.Listener that listens on an in memory network.
type memoryListener struct {
	addr memoryAddr

	conns  chan net.Conn
	once   sync.Once
	closed chan struct{}
}

// newMemoryListener returns a new in-memory listener.
func newMemoryListener(addr string) *memoryListener {
	return &memoryListener{
		addr:   memoryAddr(addr),
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

// Accept implements net.Listener.
func (l *memoryListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, &net.OpError{
			Op:   "accept",
			Net:  l.addr.Network(),
			Addr: l.addr,
			Err:  errListenerClosed,
		}
	case server := <-l.conns:
		return server, nil
	}
}

// Close implements net.Listener.
func (l *memoryListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
	})
	return nil
}

// Addr implements net.Listener.
func (l *memoryListener) Addr() net.Addr {
	return l.addr
}

// DialContext is the type expected by http.Transport.DialContext.
func (l *memoryListener) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return l.dialContext(ctx, false)
}

// DialContextBuffered is like DialContext but wraps both sides of the
// connection with asynchronous write buffering. This mimics the kernel-level
// write buffering of real TCP connections and prevents deadlocks in HTTP/1.x
// when both sides write concurrently (e.g., the server's finishRequest
// flushes the chunked response while the client is still writing the request
// body). HTTP/2 connections should use DialContext instead, as the buffering
// can delay write-failure propagation and interfere with disconnect detection.
func (l *memoryListener) DialContextBuffered(ctx context.Context, network, addr string) (net.Conn, error) {
	return l.dialContext(ctx, true)
}

func (l *memoryListener) dialContext(ctx context.Context, buffered bool) (net.Conn, error) {
	server, client := net.Pipe()
	if buffered {
		server = newBufferedPipeConn(server)
		client = newBufferedPipeConn(client)
	}
	select {
	case <-ctx.Done():
		return nil, &net.OpError{Op: "dial", Net: l.addr.Network(), Err: ctx.Err()}
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		return nil, &net.OpError{Op: "dial", Net: l.addr.Network(), Err: errListenerClosed}
	}
}

type memoryAddr string

// Network implements net.Addr.
func (memoryAddr) Network() string { return "memory" }

// String implements io.Stringer, returning a value that matches the
// certificates used by net/http/httptest.
func (a memoryAddr) String() string { return string(a) }
