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

package connect

import (
	"errors"
	"testing"

	"connectrpc.com/connect/internal/assert"
)

func TestErrStreamingClientConnHeadersAreStable(t *testing.T) {
	t.Parallel()

	conn := newErrStreamingClientConn(errors.New("boom"))

	// A value written through one call to a header accessor must be visible
	// through a subsequent call to the same accessor. Each call must return the
	// same underlying http.Header map.
	conn.RequestHeader().Set("X-Trace-ID", "abc-123")
	assert.Equal(t, conn.RequestHeader().Get("X-Trace-ID"), "abc-123")

	conn.ResponseHeader().Set("X-Response", "value-1")
	assert.Equal(t, conn.ResponseHeader().Get("X-Response"), "value-1")

	conn.ResponseTrailer().Set("X-Trailer", "value-2")
	assert.Equal(t, conn.ResponseTrailer().Get("X-Trailer"), "value-2")
}

func TestErrStreamingClientConnReturnsError(t *testing.T) {
	t.Parallel()

	err := errors.New("boom")
	conn := newErrStreamingClientConn(err)

	var msg any
	assert.ErrorIs(t, conn.Receive(&msg), err)
	assert.ErrorIs(t, conn.Send(&msg), err)
	assert.ErrorIs(t, conn.CloseRequest(), err)
	assert.ErrorIs(t, conn.CloseResponse(), err)
	assert.Equal(t, conn.Spec(), Spec{})
	assert.Equal(t, conn.Peer(), Peer{})
}
