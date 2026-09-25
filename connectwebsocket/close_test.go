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

package connectwebsocket

import (
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
)

// deadSender fails every write, standing in for a socket whose peer has gone.
// A real disconnect cannot be provoked reliably — whether a write lands in a
// buffer or meets a reset is a matter of timing — so the condition is supplied
// directly here.
type deadSender struct{}

func (deadSender) reason() string {
	return "write tcp 127.0.0.1:8080->127.0.0.1:1234: write: broken pipe"
}

// A verdict that cannot be delivered because the client already left is not a
// server error. Reporting it would log one for every abandoned subscription.
func TestCloseSwallowsTheWriteErrorWhenThePeerIsGone(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		sawEnd      bool
		eof         bool
		wantSwallow bool
	}{
		{
			name:        "peer left without C",
			eof:         true,
			wantSwallow: true,
		},
		{
			name:   "peer sent C and is waiting for the verdict",
			eof:    true,
			sawEnd: true,
		},
		{
			name: "read path never reached the end, so nothing says the peer left",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			conn := &websocketHandlerConn{
				unmarshaler: websocketUnmarshaler{
					eof:                  test.eof,
					sawEndOfClientStream: test.sawEnd,
				},
			}
			writeErr := errorf(connect.CodeUnknown, "write message: %s", deadSender{}.reason())
			got := conn.terminalWriteError(writeErr)
			if test.wantSwallow {
				assert.Nil(t, got)
				return
			}
			assert.NotNil(t, got)
			assert.Equal(t, got.Code(), connect.CodeUnknown)
		})
	}
}
