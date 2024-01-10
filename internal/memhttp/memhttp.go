// Copyright 2021-2024 The Connect Authors
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
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Server is a net/http server that uses in-memory pipes instead of TCP. By
// default, it supports http/2 via h2c. It otherwise uses the same configuration
// as the zero value of [http.Server].
type Server struct {
	server         http.Server
	listener       *memoryListener
	url            string
	cleanupTimeout time.Duration

	serverWG  sync.WaitGroup
	serverErr error
}

// NewServer creates a new Server that uses the given handler. Configuration
// options may be provided via [Option]s.
func NewServer(handler http.Handler, opts ...Option) *Server {
	var cfg config
	WithCleanupTimeout(5 * time.Second).apply(&cfg)
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	h2s := &http2.Server{}
	handler = h2c.NewHandler(handler, h2s)
	listener := newMemoryListener("1.2.3.4") // httptest.DefaultRemoteAddr
	server := &Server{
		server: http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		},
		listener:       listener,
		url:            "http://" + listener.Addr().String(),
		cleanupTimeout: cfg.CleanupTimeout,
	}
	server.serverWG.Add(1)
	go func() {
		defer server.serverWG.Done()
		server.serverErr = server.server.Serve(server.listener)
	}()
	return server
}

// Transport returns a [http2.Transport] configured to use in-memory pipes
// rather than TCP and speak both HTTP/1.1 and HTTP/2.
//
// Callers may reconfigure the returned transport without affecting other transports.
func (s *Server) Transport() *http2.Transport {
	return &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			return s.listener.DialContext(ctx, network, addr)
		},
		AllowHTTP: true,
	}
}

// TransportHTTP1 returns a [http.Transport] configured to use in-memory pipes
// rather than TCP and speak HTTP/1.1.
//
// Callers may reconfigure the returned transport without affecting other transports.
func (s *Server) TransportHTTP1() *http.Transport {
	return &http.Transport{
		DialContext: s.listener.DialContext,
		// TODO(emcfarlane): DisableKeepAlives false can causes tests
		// to hang on shutdown.
		DisableKeepAlives: true,
	}
}

// Client returns an [http.Client] configured to use in-memory pipes rather
// than TCP and speak HTTP/2. It is configured to use the same
// [http2.Transport] as [Transport].
//
// Callers may reconfigure the returned client without affecting other clients.
func (s *Server) Client() *http.Client {
	return &http.Client{Transport: s.Transport()}
}

// URL returns the server's URL.
func (s *Server) URL() string {
	return s.url
}

// Shutdown gracefully shuts down the server, without interrupting any active
// connections. See [http.Server.Shutdown] for details.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.server.Shutdown(ctx); err != nil {
		return err
	}
	return s.Wait()
}

// Cleanup calls shutdown with a background context set with the cleanup timeout.
// The default timeout duration is 5 seconds.
func (s *Server) Cleanup() error {
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, s.cleanupTimeout)
	defer cancel()
	return s.Shutdown(ctx)
}

// Close closes the server's listener. It does not wait for connections to
// finish.
func (s *Server) Close() error {
	return s.server.Close()
}

// RegisterOnShutdown registers a function to call on Shutdown. See
// [http.Server.RegisterOnShutdown] for details.
func (s *Server) RegisterOnShutdown(f func()) {
	s.server.RegisterOnShutdown(f)
}

// Wait blocks until the server exits, then returns an error if not
// a [http.ErrServerClosed] error.
func (s *Server) Wait() error {
	s.serverWG.Wait()
	if !errors.Is(s.serverErr, http.ErrServerClosed) {
		return s.serverErr
	}
	return nil
}
