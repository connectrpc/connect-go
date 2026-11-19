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

// clientStream adapts [websocketClientConn] to [connect.ClientStream].
type clientStream struct {
	conn           *websocketClientConn
	singleResponse bool
	// received marks the one response a single-response call has read.
	received bool
}

func (s *clientStream) SendHeaders() error {
	return s.conn.SendHeaders()
}

func (s *clientStream) Send(msg any) error {
	return s.conn.Send(msg)
}

func (s *clientStream) CloseSend() error {
	return s.conn.CloseRequest()
}

func (s *clientStream) Receive(msg any) error {
	err := s.conn.Receive(msg)
	if err != nil {
		// Reading to completion is what makes response metadata available.
		s.conn.publishMetadata()
		// §6.2: a unary or client-streaming call returns exactly one message,
		// so a successful end with none is a cardinality error, as over HTTP.
		if s.singleResponse && !s.received && errors.Is(err, io.EOF) {
			return connect.Errorf(connect.CodeUnimplemented, "unary stream has zero messages")
		}
		return err
	}
	// Leading metadata has arrived by now, and connecthttp makes it readable at
	// exactly this point: after the first Receive, not before.
	s.conn.publishHeaders()
	if s.singleResponse {
		s.received = true
		// Such a caller receives exactly once, so nothing would ever read the
		// end-of-stream message — and the trailers it carries would be lost. A
		// server-streaming caller reaches it by reading to io.EOF. A second
		// message where the S belongs is decoded into msg only to be refused.
		endErr := s.conn.Receive(msg)
		s.conn.publishMetadata()
		if endErr == nil {
			return connect.Errorf(connect.CodeUnimplemented, "unary stream has multiple messages")
		}
		if !errors.Is(endErr, io.EOF) {
			return endErr
		}
	}
	return nil
}

// Close ends the stream without sending C: a client that closes before S
// without sending C cancels the RPC (§8), and one that has S needs no C. A
// stream that never dialed is closed without dialing.
func (s *clientStream) Close() error {
	dialed := s.conn.preventDial()
	s.conn.publishMetadata()
	if !dialed {
		return nil
	}
	return s.conn.CloseResponse()
}
