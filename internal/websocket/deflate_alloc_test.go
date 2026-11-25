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

//go:build !race

package websocket

import (
	"bufio"
	"bytes"
	"fmt"
	"testing"

	"connectrpc.com/connect/v2/internal/assert"
)

// TestZeroAllocsCompressed measures compressed messages with warm pools.
// The race detector makes sync.Pool drop items at random, which is why this
// file builds without it.
// flate allocates the overflow links of a dynamic Huffman table with codes
// over 9 bits, which a large message's blocks need. Reading 64 KiB of JSON
// costs 7 allocations with Go 1.27.
//
//nolint:paralleltest // AllocsPerRun panics in parallel tests
func TestZeroAllocsCompressed(t *testing.T) {
	payloads := map[string][]byte{
		"repeated": bytes.Repeat([]byte("x"), 1024),
		"json":     jsonPayload(1 << 10),
		"json_64k": jsonPayload(64 << 10),
	}
	maxReadAllocs := map[string]float64{"json_64k": 16}
	for name, payload := range payloads {
		for _, client := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/client_%t", name, client), func(t *testing.T) {
				var stream bytes.Buffer
				peer := newDeflateConn(&bufferTransport{buffer: &stream}, nil, !client, true, Options{})
				assert.Nil(t, peer.WriteMessage(MessageText, frameOf(payload)))
				assert.True(t, stream.Bytes()[0]&rsv1 != 0)
				transport := &loopTransport{data: stream.Bytes()}
				conn := newDeflateConn(transport, bufio.NewReader(transport), client, true, Options{})
				var dst bytes.Buffer
				read := testing.AllocsPerRun(100, func() {
					dst.Reset()
					_, err := conn.ReadMessage(&dst, 1<<20)
					assert.Nil(t, err)
				})
				assert.Equal(t, dst.Bytes(), payload)
				frame := frameOf(payload)
				write := testing.AllocsPerRun(100, func() {
					assert.Nil(t, conn.WriteMessage(MessageText, frame))
				})
				assert.Equal(t, write, 0)
				assert.True(t, read <= maxReadAllocs[name], assert.Sprintf("read %v allocs", read))
			})
		}
	}
}
