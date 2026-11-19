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

package connecthttp

import (
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
)

// Options appended for one spec must not land in another's, which they would
// if two clones shared a backing array with room to grow.
func TestForSpecClonesWebSocketOptions(t *testing.T) {
	t.Parallel()
	opts := defaultOptions()
	WithWebSocketKeepAlive(time.Second).apply(&opts)
	opts.websocketServerOptions = slices.Grow(opts.websocketServerOptions, 4)
	opts.websocketClientOptions = slices.Grow(opts.websocketClientOptions, 4)
	WithConditionalOptions(func(connect.Spec) []Option {
		return []Option{WithWebSocketKeepAliveTimeout(time.Minute)}
	}).apply(&opts)

	first := opts.forSpec(connect.Spec{Procedure: "/a.A/One"})
	second := opts.forSpec(connect.Spec{Procedure: "/a.A/Two"})
	assert.Equal(t, len(opts.websocketServerOptions), 1)
	assert.Equal(t, len(first.websocketServerOptions), 2)
	assert.True(t, &first.websocketServerOptions[1] != &second.websocketServerOptions[1])
	assert.True(t, &first.websocketClientOptions[1] != &second.websocketClientOptions[1])
}

// The hybrid transport's WebSocket routes and its fallback's client state are
// both per procedure. They share one resolution of the conditional options,
// so the callback runs once per procedure even when first calls race.
func TestHybridTransportResolvesConditionalOptionsOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	hybrid, ok := NewTransport(
		http.DefaultClient,
		"http://localhost",
		WithConditionalOptions(func(connect.Spec) []Option {
			calls.Add(1)
			return []Option{WithWebSocket(SelectStreaming)}
		}),
	).(*hybridTransport)
	if !ok {
		t.Fatal("conditional options must produce a hybridTransport")
	}
	spec := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/svc/A"}

	start := make(chan struct{})
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			<-start
			hybrid.route(spec)
		})
		group.Go(func() {
			<-start
			hybrid.fallback.clientStateForSpec(spec)
		})
	}
	close(start)
	group.Wait()
	// A unary call routes to the fallback, through both caches.
	_, err := hybrid.NewClientStream(t.Context(), spec)
	assert.Nil(t, err)
	assert.Equal(t, calls.Load(), int32(1))
	assert.True(t, hybrid.route(spec).selector != nil)
}
