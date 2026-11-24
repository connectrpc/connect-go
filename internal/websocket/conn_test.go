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

package websocket

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"connectrpc.com/connect/v2/internal/assert"
)

// TestCloseDiscardsPartialMessage drives the interleaving where Close drains
// the rest of a message that a concurrent read has started. That read must
// fail instead of passing off the truncated message as whole.
func TestCloseDiscardsPartialMessage(t *testing.T) {
	t.Parallel()
	input := appendHeader(nil, opBinary, 100, nil)
	input = append(input, make([]byte, 100)...)
	input = appendHeader(input, opClose, 2, nil)
	input = binary.BigEndian.AppendUint16(input, uint16(StatusNormalClosure))
	transport := &fuzzTransport{Reader: bytes.NewReader(input)}
	conn := newConn(transport, bufio.NewReader(transport), bufio.NewWriter(io.Discard), true)
	_, reader, err := conn.Reader(t.Context())
	assert.Nil(t, err)
	_, err = io.ReadFull(reader, make([]byte, 10))
	assert.Nil(t, err)
	// Close sets closing before the handshake and closes the transport only
	// after it, which leaves a window for the read below.
	conn.closing.Store(true)
	assert.Nil(t, conn.closeHandshake(StatusNormalClosure, ""))
	rest, err := io.ReadAll(reader)
	assert.ErrorIs(t, err, net.ErrClosed)
	assert.Equal(t, len(rest), 0)
}
