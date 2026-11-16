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
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
)

func TestTransportCachesClientState(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	transport, ok := NewTransport(
		http.DefaultClient,
		"http://localhost",
		WithConditionalOptions(func(spec connect.Spec) []Option {
			calls.Add(1)
			if spec.Procedure == "/svc/B" {
				return []Option{WithReadMaxBytes(1)}
			}
			return nil
		}),
	).(*transport)
	assert.True(t, ok)
	// The cache keys by procedure. This Schema would panic as a map key.
	specA := connect.Spec{
		StreamType: connect.StreamTypeUnary,
		Procedure:  "/svc/A",
		Schema:     struct{ fields any }{[]string{"a"}},
	}
	specB := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/svc/B"}
	// Concurrent first calls must resolve once.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			<-start
			transport.clientStateForSpec(specA)
		})
	}
	close(start)
	wg.Wait()
	stateA := transport.clientStateForSpec(specA)
	assert.Nil(t, stateA.err)
	stateB := transport.clientStateForSpec(specB)
	assert.Nil(t, stateB.err)
	assert.NotEqual(t, stateA.opts.readMaxBytes, 1)
	assert.Equal(t, stateB.opts.readMaxBytes, 1)
	stream, err := transport.NewClientStream(t.Context(), specA)
	assert.Nil(t, err)
	unary, ok := stream.(*connectUnaryClientStream)
	assert.True(t, ok)
	assert.True(t, unary.protoClient == stateA.client)
	assert.Equal(t, calls.Load(), 2)
}

func TestTransportCachesClientStateErrors(t *testing.T) {
	t.Parallel()
	transport, ok := NewTransport(http.DefaultClient, "http://[bad").(*transport)
	assert.True(t, ok)
	spec := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/svc/A"}
	_, err := transport.NewClientStream(t.Context(), spec)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable)
	assert.True(t, errors.Is(err, transport.clientStateForSpec(spec).err))
}

func TestTransportCachedClientStateNoAllocs(t *testing.T) { //nolint:paralleltest // AllocsPerRun can't run in parallel
	transport, ok := NewTransport(http.DefaultClient, "http://localhost").(*transport)
	assert.True(t, ok)
	spec := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/svc/A"}
	transport.clientStateForSpec(spec)
	allocs := testing.AllocsPerRun(100, func() {
		transport.clientStateForSpec(spec)
	})
	assert.Equal(t, allocs, 0)
}
