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

// closeWatchingServer accepts one upgrade, opens the stream with an empty M,
// and reports the code of the client's Close frame.
func closeWatchingServer(tb testing.TB) (string, <-chan int) {
	tb.Helper()
	listener, err := (&net.ListenConfig{}).Listen(tb.Context(), "tcp", "127.0.0.1:0")
	assert.Nil(tb, err)
	tb.Cleanup(func() { _ = listener.Close() })
	closes := make(chan int, 1)
	go func() {
		conn, reader := acceptRawUpgrade(tb, listener)
		if _, err := conn.Write(serverFrame(true, []byte("M[]"))); err != nil {
			return
		}
		_, closeCode, _ := watchFrames(reader, nil)
		closes <- closeCode
	}()
	return "http://" + listener.Addr().String(), closes
}

// §11.1 and §13: a client whose call ends before S, by cancellation or by its
// deadline, closes with 1000 rather than dropping the connection, so the
// server can tell it was canceled.
func TestClientClosesWithNormalClosureWhenItsCallEnds(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		end      func(context.CancelFunc)
		timeout  time.Duration
		wantCode connect.Code
	}{
		{name: "canceled", end: func(cancel context.CancelFunc) { cancel() }, wantCode: connect.CodeCanceled},
		{name: "deadline", end: func(context.CancelFunc) {}, timeout: 100 * time.Millisecond, wantCode: connect.CodeDeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			url, closes := closeWatchingServer(t)
			transport, err := wstransport.NewTransport(url)
			assert.Nil(t, err)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.timeout > 0 {
				ctx, cancel = context.WithTimeout(t.Context(), test.timeout)
				defer cancel()
			}
			stream, err := client.CumSum(ctx)
			assert.Nil(t, err)
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
			test.end(cancel)
			_, err = stream.Receive()
			assert.Equal(t, connect.CodeOf(err), test.wantCode)

			select {
			case closeCode := <-closes:
				assert.Equal(t, closeCode, 1000)
			case <-time.After(wsTestReadTimeout):
				t.Fatal("server never saw the connection end")
			}
		})
	}
}
