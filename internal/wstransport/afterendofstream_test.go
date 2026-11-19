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
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect/v2/internal/assert"
	"github.com/coder/websocket"
)

// recordingSender keeps every message written through it.
type recordingSender struct {
	mu       sync.Mutex
	messages [][]byte
}

func (s *recordingSender) send(_ context.Context, _ bool, data []byte) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, append([]byte(nil), data...))
	return int64(len(data)), nil
}

func (s *recordingSender) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		return ""
	}
	return string(s.messages[len(s.messages)-1])
}

// §8: a message the server has already read after C is a protocol error, even
// when the handler returned at io.EOF before the post-C drain looked at it.
// Close is the last place it can be noticed, and must not report success.
func TestCloseReportsAMessageReadAfterEndOfClientStream(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		read      frameResult
		wantCode  websocket.StatusCode
		wantInEnd string
	}{
		{
			name:      "message",
			read:      frameResult{messageType: websocket.MessageBinary, frame: []byte{markerBody}},
			wantCode:  websocket.StatusProtocolError,
			wantInEnd: "after its C message",
		},
		{
			name:      "oversized message",
			read:      frameResult{err: errMessageTooBig},
			wantCode:  websocket.StatusMessageTooBig,
			wantInEnd: "resource_exhausted",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			delivered := false
			reader := newFrameReader(ctx, func(readCtx context.Context) (websocket.MessageType, []byte, error) {
				if !delivered {
					delivered = true
					return test.read.messageType, test.read.frame, test.read.err
				}
				<-readCtx.Done()
				return 0, nil, readCtx.Err()
			}, nil)
			t.Cleanup(func() {
				cancel()
				reader.close()
			})
			assert.True(t, reader.await(ctx))

			serverConn, clientConn := connPair(t)
			sender := &recordingSender{}
			conn := &websocketHandlerConn{
				wsConn: serverConn,
				marshaler: messageWriter{
					sender: sender,
				},
				frameDecoder:         frameDecoder{readMaxBytes: 1024},
				reader:               reader,
				eof:                  true,
				sawEndOfClientStream: true,
				leadingSent:          true,
			}
			closed := make(chan error, 1)
			go func() { closed <- conn.Close(t.Context(), nil) }()

			readCtx, cancelRead := context.WithTimeout(t.Context(), wsTestReadTimeout)
			defer cancelRead()
			_, _, readErr := clientConn.Read(readCtx)
			assert.Equal(t, websocket.CloseStatus(readErr), test.wantCode)
			<-closed
			end := sender.last()
			assert.True(t, strings.HasPrefix(end, "S"), assert.Sprintf("%q", end))
			assert.True(t, strings.Contains(end, test.wantInEnd), assert.Sprintf("%q", end))
		})
	}
}
