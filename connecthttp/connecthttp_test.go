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
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
)

func TestTransportReusesClientState(t *testing.T) {
	t.Parallel()
	transport, ok := NewTransport(http.DefaultClient, "http://localhost").(*transport)
	assert.True(t, ok)
	stateFor := func(procedure string) *clientState {
		t.Helper()
		state := transport.clientStateForSpec(connect.Spec{Procedure: procedure})
		assert.Nil(t, state.err)
		return state
	}
	first := stateFor("/svc/A")
	assert.True(t, first == stateFor("/svc/A"))
	assert.False(t, first == stateFor("/svc/B"))
	// Calls through NewClientStream share the cached protocol client.
	stream, err := transport.NewClientStream(t.Context(), connect.Spec{
		StreamType: connect.StreamTypeUnary,
		Procedure:  "/svc/A",
	})
	assert.Nil(t, err)
	unary, ok := stream.(*connectUnaryClientStream)
	assert.True(t, ok)
	assert.True(t, unary.protoClient == first.client)
}

func TestTransportCachesClientStateErrors(t *testing.T) {
	t.Parallel()
	transport, ok := NewTransport(http.DefaultClient, "http://[bad").(*transport)
	assert.True(t, ok)
	spec := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/svc/A"}
	_, err := transport.NewClientStream(t.Context(), spec)
	assert.Equal(t, connect.CodeOf(err), connect.CodeUnavailable)
	first := transport.clientStateForSpec(spec)
	assert.NotNil(t, first.err)
	assert.True(t, first == transport.clientStateForSpec(spec))
}

func TestTransportConditionalOptionsKeyedBySpec(t *testing.T) {
	t.Parallel()
	var calls int
	transport, ok := NewTransport(
		http.DefaultClient,
		"http://localhost",
		WithConditionalOptions(func(spec connect.Spec) []Option {
			calls++
			if spec.StreamType == connect.StreamTypeServer {
				return []Option{WithReadMaxBytes(1)}
			}
			return nil
		}),
	).(*transport)
	assert.True(t, ok)
	unarySpec := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/svc/A"}
	serverSpec := connect.Spec{StreamType: connect.StreamTypeServer, Procedure: "/svc/A"}
	for range 3 {
		assert.NotEqual(t, transport.clientStateForSpec(unarySpec).opts.readMaxBytes, 1)
		assert.Equal(t, transport.clientStateForSpec(serverSpec).opts.readMaxBytes, 1)
	}
	assert.Equal(t, calls, 2)
	// A Schema that is not comparable cannot key the cache and still works.
	spec := unarySpec
	spec.Schema = []string{"schema"}
	assert.Nil(t, transport.clientStateForSpec(spec).err)
	assert.Nil(t, transport.clientStateForSpec(spec).err)
	assert.Equal(t, calls, 4)
}
