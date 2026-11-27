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

func TestCallServerStreamClosesStreamOnError(t *testing.T) {
	t.Parallel()
	errSend := errors.New("send failed")
	errCloseSend := errors.New("close send failed")
	errClose := errors.New("close failed")
	spec := connect.Spec{
		StreamType: connect.StreamTypeServer,
		Procedure:  "/test.v1.Service/ServerStream",
	}

	testCases := []struct {
		name         string
		sendErr      error
		closeSendErr error
		closeErr     error
		wantStream   bool
		wantClosed   int
		wantErrs     []error
	}{
		{
			name:       "send_error",
			sendErr:    errSend,
			wantClosed: 1,
			wantErrs:   []error{errSend},
		},
		{
			name:       "send_error_joins_close_error",
			sendErr:    errSend,
			closeErr:   errClose,
			wantClosed: 1,
			wantErrs:   []error{errSend, errClose},
		},
		{
			name:         "close_send_error",
			closeSendErr: errCloseSend,
			wantClosed:   1,
			wantErrs:     []error{errCloseSend},
		},
		{
			name:         "close_send_error_joins_close_error",
			closeSendErr: errCloseSend,
			closeErr:     errClose,
			wantClosed:   1,
			wantErrs:     []error{errCloseSend, errClose},
		},
		{
			name:       "send_eof_keeps_stream_open",
			sendErr:    io.EOF,
			wantStream: true,
			wantClosed: 0,
		},
		{
			name:         "send_eof_then_close_send_error",
			sendErr:      io.EOF,
			closeSendErr: errCloseSend,
			wantClosed:   1,
			wantErrs:     []error{errCloseSend},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubClientStream{
				sendErr:      testCase.sendErr,
				closeSendErr: testCase.closeSendErr,
				closeErr:     testCase.closeErr,
			}
			client := connect.NewClient(&stubTransport{stream: stub})

			gotStream, gotErr := client.CallServerStream(t.Context(), spec, "request")
			if stub.closed != testCase.wantClosed {
				t.Fatalf("stream Close() count = %d, want %d", stub.closed, testCase.wantClosed)
			}
			if testCase.wantStream {
				if gotErr != nil {
					t.Fatalf("CallServerStream returned unexpected error: %v", gotErr)
				}
				if gotStream != stub {
					t.Fatalf("CallServerStream returned stream %v, want %v", gotStream, stub)
				}
				return
			}
			if gotStream != nil {
				t.Fatalf("CallServerStream returned non-nil stream on error: %v", gotStream)
			}
			if gotErr == nil {
				t.Fatal("CallServerStream returned nil error, want non-nil")
			}
			for _, wantErr := range testCase.wantErrs {
				if !errors.Is(gotErr, wantErr) {
					t.Fatalf("CallServerStream error = %v, want errors.Is(_, %v)", gotErr, wantErr)
				}
			}
		})
	}
}

type stubTransport struct {
	stream connect.ClientStream
}

func (s *stubTransport) NewClientStream(_ context.Context, _ connect.Spec) (connect.ClientStream, error) {
	return s.stream, nil
}

type stubClientStream struct {
	sendErr      error
	closeSendErr error
	closeErr     error
	closed       int
}

func (s *stubClientStream) SendHeaders() error { return nil }

func (s *stubClientStream) Send(_ any) error { return s.sendErr }

func (s *stubClientStream) CloseSend() error { return s.closeSendErr }

func (s *stubClientStream) Receive(_ any) error { return io.EOF }

func (s *stubClientStream) Close() error {
	s.closed++
	return s.closeErr
}
