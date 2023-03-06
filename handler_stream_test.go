// Copyright 2021-2023 Buf Technologies, Inc.
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

package connect

import (
	"fmt"
	"testing"

	"github.com/bufbuild/connect-go/internal/assert"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
)

func TestClientStream(t *testing.T) {
	t.Parallel()
	clientStream := &ClientStream[pingv1.PingRequest]{conn: &nopClientStreamingHandlerConn{}}
	assert.True(t, clientStream.Receive())
	first := fmt.Sprintf("%p", clientStream.Msg())
	assert.True(t, clientStream.Receive())
	second := fmt.Sprintf("%p", clientStream.Msg())
	assert.NotEqual(t, first, second)
}

type nopClientStreamingHandlerConn struct {
	StreamingHandlerConn
}

func (nopClientStreamingHandlerConn) Receive(msg any) error {
	return nil
}
