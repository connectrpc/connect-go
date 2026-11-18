// Copyright 2021-2026 The Connect Authors
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

package wstransport

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

// errServerShutdown is the cause a session's context carries once the
// [http.Server] serving it begins to shut down.
var errServerShutdown = errors.New("server is shutting down")

// shutdownTracker ends sessions when their server shuts down. A hijacked
// connection is invisible to [http.Server.Shutdown], so without this a stream
// outlives its server with no S message (§13).
type shutdownTracker struct {
	mu         sync.Mutex
	registered map[*http.Server]struct{}
	sessions   map[*trackedSession]struct{}
}

type trackedSession struct {
	server *http.Server
	cancel context.CancelCauseFunc
}

func newShutdownTracker() *shutdownTracker {
	return &shutdownTracker{
		registered: make(map[*http.Server]struct{}),
		sessions:   make(map[*trackedSession]struct{}),
	}
}

// track returns the context to serve a session under, derived from the
// request's ctx and canceled with errServerShutdown when the server that
// accepted the request shuts down. The returned function stops tracking it.
func (t *shutdownTracker) track(ctx context.Context) (context.Context, func()) {
	server, ok := ctx.Value(http.ServerContextKey).(*http.Server)
	ctx, cancel := context.WithCancelCause(ctx)
	if !ok || server == nil {
		return ctx, func() { cancel(nil) }
	}
	session := &trackedSession{server: server, cancel: cancel}
	t.mu.Lock()
	if _, registered := t.registered[server]; !registered {
		t.registered[server] = struct{}{}
		server.RegisterOnShutdown(func() { t.shutdown(server) })
	}
	t.sessions[session] = struct{}{}
	t.mu.Unlock()
	return ctx, func() {
		t.mu.Lock()
		delete(t.sessions, session)
		t.mu.Unlock()
		cancel(nil)
	}
}

func (t *shutdownTracker) shutdown(server *http.Server) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.registered, server)
	for session := range t.sessions {
		if session.server == server {
			session.cancel(errServerShutdown)
		}
	}
}

// isShuttingDown reports whether ctx was canceled by its server shutting down.
func isShuttingDown(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errServerShutdown)
}
