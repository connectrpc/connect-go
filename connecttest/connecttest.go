// Package connecttest contains testing utilities for connect, including a
// replacement for httptest.Server.
package connecttest

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
	server   *httptest.Server
	listener *memoryListener
}

// NewServer constructs and starts a Server.
func NewServer(handler http.Handler) *Server {
	lis := &memoryListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = lis
	server.EnableHTTP2 = true
	server.StartTLS()
	return &Server{
		server:   server,
		listener: lis,
	}
}

// Client returns an HTTP client configured to trust the server's TLS
// certificate and use HTTP/2 over an in-memory pipe. Automatic HTTP-level gzip
// compression is disabled. It closes its idle connections when the server is
// closed.
func (s *Server) Client() *http.Client {
	client := s.server.Client()
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.DialContext = s.listener.DialContext
		transport.DisableCompression = true
	}
	return client
}

// URL is the server's URL.
func (s *Server) URL() string {
	return s.server.URL
}

// Close shuts down the server, blocking until all outstanding requests have
// completed.
func (s *Server) Close() {
	s.server.Close()
}

type memoryListener struct {
	conns  chan net.Conn
	once   sync.Once
	closed chan struct{}
}

// Accept implements net.Listener.
func (l *memoryListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
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
