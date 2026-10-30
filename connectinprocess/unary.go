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

package connectinprocess

import (
	"errors"
	"io"

	"connectrpc.com/connect/v2"
)

// unaryClientStream runs a unary or client-streaming RPC on a streamPair.
// Receive reads the single response and then waits for the server to return.
// A cardinality violation maps to CodeUnimplemented as it does in connecthttp.
type unaryClientStream struct {
	clientStream

	rxEnd bool
}

func (s *unaryClientStream) Receive(dst any) error {
	if s.rxEnd {
		return io.EOF
	}
	s.rxEnd = true
	err := s.clientStream.Receive(dst)
	if err == nil {
		if drainErr := s.clientStream.Receive(dst); drainErr == nil {
			err = connect.Errorf(connect.CodeUnimplemented, "unary stream has multiple messages")
		} else if !errors.Is(drainErr, io.EOF) {
			err = drainErr
		}
	} else if errors.Is(err, io.EOF) {
		err = connect.Errorf(connect.CodeUnimplemented, "unary stream has zero messages")
	}
	return err
}
