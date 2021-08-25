// Package retest contains testing utilities for reRPC, including a replacement for httptest.Server.
package retest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Server is an HTTP server that uses in-memory pipes instead of TCP. It
// supports HTTP/2 and has TLS enabled.
//
// It's intended to be a preconfigured, faster alternative to the standard
// library's httptest.Server.
type Server struct {
	srv *httptest.Server
	lis *memoryListener
}

// NewServer constructs and starts a Server.
func NewServer(h http.Handler) *Server {
	lis := &memoryListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = lis
	srv.EnableHTTP2 = true
	srv.StartTLS()
	return &Server{
		srv: srv,
		lis: lis,
	}
}

// Client returns an HTTP client configured to trust the server's TLS
// certificate and use HTTP/2 over an in-memory pipe. It closes its idle
// connections when the server is closed.
func (s *Server) Client() *http.Client {
	c := s.srv.Client()
	if tr, ok := c.Transport.(*http.Transport); ok {
		tr.DialContext = s.lis.DialContext
	}
	return c
}

// URL is the server's URL.
func (s *Server) URL() string {
	return s.srv.URL
}

// Close shuts down the server, blocking until all outstanding requests have
// completed.
func (s *Server) Close() {
	s.srv.Close()
}

type memoryListener struct {
	conns  chan net.Conn
	once   sync.Once
	closed chan struct{}
}

// Accept implements net.Listener.
func (l *memoryListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, errors.New("listener closed")
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
	return &memoryAddr{}
}

// DialContext is the type expected by http.Transport.DialContext.
func (l *memoryListener) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, errors.New("listener closed")
	default:
	}
	server, client := net.Pipe()
	l.conns <- server
	return client, nil
}

type memoryAddr struct{}

// Network implements net.Addr.
func (*memoryAddr) Network() string { return "memory" }

// String implements io.Stringer, returning a value that matches the
// certificates used by net/http/httptest.
func (*memoryAddr) String() string { return "example.com" }
