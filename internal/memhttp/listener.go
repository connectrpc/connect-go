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

package memhttp

import (
	"context"
	"errors"
	"net"
	"sync"
)

// MemoryListener is a net.Listener that listens on an in memory network.
type MemoryListener struct {
	conns  chan net.Conn
	once   sync.Once
	closed chan struct{}
}

// NewMemoryListener returns a new in-memory listener.
func NewMemoryListener() *MemoryListener {
	return &MemoryListener{
		conns:  make(chan net.Conn),
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
	case server := <-l.conns:
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
	server, client := net.Pipe()
	select {
	case <-ctx.Done():
		return nil, derr(ctx.Err())
	case l.conns <- server:
		return client, nil
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
