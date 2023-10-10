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
	listener       *MemoryListener
	url            string
	cleanupTimeout time.Duration
	disableHTTP2   bool

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

	if !cfg.DisableHTTP2 {
		h2s := &http2.Server{}
		handler = h2c.NewHandler(handler, h2s)
	}
	listener := NewMemoryListener()
	server := &Server{
		server: http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		},
		listener:       listener,
		cleanupTimeout: cfg.CleanupTimeout,
		url:            "http://" + listener.Addr().String(),
		disableHTTP2:   cfg.DisableHTTP2,
	}
	server.goServe()
	return server
}

// Transport returns a [http.RoundTripper] configured to use in-memory pipes
// rather than TCP and talk HTTP/2 (if the server supports it).
func (s *Server) Transport() http.RoundTripper {
	if s.disableHTTP2 {
		return &http.Transport{
			DialContext: s.listener.DialContext,
		}
	}
	return &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			return s.listener.DialContext(ctx, network, addr)
		},
		AllowHTTP: true,
	}
}

// Client returns an [http.Client] configured to use in-memory pipes rather
// than TCP, disable automatic compression, trust the server's TLS certificate
// (if any), and use HTTP/2 (if the server supports it).
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
	ctx, cancel := context.WithTimeout(ctx, s.cleanupTimeout)
	defer cancel()
	if err := s.server.Shutdown(ctx); err != nil {
		return err
	}
	return s.Wait()
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

// Wait blocks until the server exits, then returns its error.
func (s *Server) Wait() error {
	s.serverWG.Wait()
	if !errors.Is(s.serverErr, http.ErrServerClosed) {
		return s.serverErr
	}
	return nil
}

func (s *Server) goServe() {
	s.serverWG.Add(1)
	go func() {
		defer s.serverWG.Done()
		s.serverErr = s.server.Serve(s.listener)
	}()
}
