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
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
)

func TestTransportCachesClientState(t *testing.T) {
	t.Parallel()
	var calls int
	transport, ok := NewTransport(
		http.DefaultClient,
		"http://localhost",
		WithConditionalOptions(func(spec connect.Spec) []Option {
			calls++
			if spec.Procedure == "/svc/B" {
				return []Option{WithReadMaxBytes(1)}
			}
			return nil
		}),
	).(*transport)
	assert.True(t, ok)
	// The Schema type is comparable but its value can't be hashed.
	specA := connect.Spec{
		StreamType: connect.StreamTypeUnary,
		Procedure:  "/svc/A",
		Schema:     struct{ fields any }{[]string{"a"}},
	}
	stateA := transport.clientStateForSpec(specA)
	assert.Nil(t, stateA.err)
	assert.True(t, stateA == transport.clientStateForSpec(specA))
	stateB := transport.clientStateForSpec(connect.Spec{Procedure: "/svc/B"})
	assert.NotEqual(t, stateA.opts.readMaxBytes, 1)
	assert.Equal(t, stateB.opts.readMaxBytes, 1)
	stream, err := transport.NewClientStream(t.Context(), specA)
	assert.Nil(t, err)
	unary, ok := stream.(*connectUnaryClientStream)
	assert.True(t, ok)
	assert.True(t, unary.protoClient == stateA.client)
	assert.Equal(t, calls, 2)
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
