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

package wstransport_test

import (
	"context"
	"net"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/wstransport"
)

// §5: a client that reads a protocol error from the server fails the RPC and
// closes with 3102, the client's counterpart of 1002, without being asked to.
// §6.1 has it do the same for invalid UTF-8, as a browser would.
func TestClientClosesFaultedServerWithCode(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		frame []byte
	}{
		// Server frames are unmasked: FIN | text, then the length.
		{name: "unknown marker", frame: []byte{0x81, 3, 'Z', '{', '}'}},
		{name: "invalid UTF-8", frame: []byte{0x81, 3, 'M', '{', 0x80}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			assert.Nil(t, err)
			t.Cleanup(func() { _ = listener.Close() })
			closeCodes := make(chan int, 1)
			go func() {
				conn, reader := acceptRawUpgrade(t, listener)
				_, _ = conn.Write([]byte{0x81, 3, 'M', '{', '}'})
				_, _ = conn.Write(test.frame)
				_, closeCode := watchFrames(reader, nil)
				closeCodes <- closeCode
				_ = conn.Close()
			}()

			transport, err := wstransport.NewTransport("http://" + listener.Addr().String())
			assert.Nil(t, err)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			stream, err := client.CumSum(ctx)
			assert.Nil(t, err)
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
			_, err = stream.Receive()
			assert.NotNil(t, err)
			go func() { _ = stream.Close() }()

			select {
			case closeCode := <-closeCodes:
				assert.Equal(t, closeCode, 3102)
			case <-ctx.Done():
				t.Fatal("client did not close the connection")
			}
		})
	}
}
