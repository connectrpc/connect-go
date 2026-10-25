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

package connect_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"connectrpc.com/connect/v2"
)

// TestClientSendEOFReportsServerError verifies the Client call helpers don't
// surface the io.EOF a stream's Send returns once the server has ended the
// RPC, so the caller sees the server's error from Receive instead.
func TestClientSendEOFReportsServerError(t *testing.T) {
	t.Parallel()
	serverErr := connect.NewError(connect.CodeUnauthenticated, "missing credentials").WithRemote()
	client := connect.NewClient(endedTransport{err: serverErr})
	t.Run("unary", func(t *testing.T) {
		t.Parallel()
		err := client.CallUnary(t.Context(), connect.Spec{StreamType: connect.StreamTypeUnary}, "req", nil)
		if !errors.Is(err, serverErr) {
			t.Errorf("CallUnary = %v, want %v", err, serverErr)
		}
	})
	t.Run("server_stream", func(t *testing.T) {
		t.Parallel()
		stream, err := client.CallServerStream(t.Context(), connect.Spec{StreamType: connect.StreamTypeServer}, "req")
		if err != nil {
			t.Fatalf("CallServerStream: %v", err)
		}
		defer stream.Close()
		if err := stream.Receive(nil); !errors.Is(err, serverErr) {
			t.Errorf("Receive = %v, want %v", err, serverErr)
		}
	})
}

// endedTransport opens streams for an RPC the server has already ended with
// err: Send reports io.EOF and Receive reports err.
type endedTransport struct {
	err error
}

func (t endedTransport) NewClientStream(context.Context, connect.Spec) (connect.ClientStream, error) {
	return endedStream(t), nil
}

type endedStream struct {
	err error
}

func (endedStream) SendHeaders() error  { return nil }
func (endedStream) Send(any) error      { return io.EOF }
func (endedStream) CloseSend() error    { return nil }
func (s endedStream) Receive(any) error { return s.err }
func (endedStream) Close() error        { return nil }
