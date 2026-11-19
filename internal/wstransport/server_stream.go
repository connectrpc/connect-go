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
	"errors"
	"io"

	"connectrpc.com/connect/v2"
)

// serverStream adapts [websocketHandlerConn] to [connect.ServerStream].
type serverStream struct {
	conn *websocketHandlerConn
	// singleRequest marks a unary or server-streaming call, whose one request
	// Receive checks is followed by C, as connecthttp does.
	singleRequest bool
	received      bool
}

func (s *serverStream) Receive(msg any) error {
	if !s.singleRequest {
		return s.conn.Receive(msg)
	}
	if s.received {
		return io.EOF
	}
	s.received = true
	if err := s.conn.Receive(msg); err != nil {
		if errors.Is(err, io.EOF) {
			return connect.Errorf(connect.CodeUnimplemented, "unary request has zero messages")
		}
		return err
	}
	// Reading on to C is also what starts the post-C reads that §8.1 requires,
	// since the handler never reads again.
	if err := s.conn.Receive(msg); err == nil {
		return connect.Errorf(connect.CodeUnimplemented, "unary request has multiple messages")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *serverStream) SendHeaders() error {
	return s.conn.SendHeaders()
}

func (s *serverStream) Send(msg any) error {
	return s.conn.Send(msg)
}
