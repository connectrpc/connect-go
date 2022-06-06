// Copyright 2021-2022 Buf Technologies, Inc.
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
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
)

func TestClientStreamForClient_NoPanics(t *testing.T) {
	t.Parallel()
	initErr := errors.New("client init failure")
	cs := &ClientStreamForClient[pingv1.PingRequest, pingv1.PingResponse]{err: initErr}
	assert.ErrorIs(t, cs.Send(&pingv1.PingRequest{}), initErr)
	verifyHeaders(t, cs.RequestHeader())
	res, err := cs.CloseAndReceive()
	assert.Nil(t, res)
	assert.ErrorIs(t, err, initErr)
}

func TestServerStreamForClient_NoPanics(t *testing.T) {
	t.Parallel()
	initErr := errors.New("client init failure")
	serverStream := &ServerStreamForClient[pingv1.PingResponse]{constructErr: initErr}
	assert.ErrorIs(t, serverStream.Err(), initErr)
	assert.ErrorIs(t, serverStream.Close(), initErr)
	assert.NotNil(t, serverStream.Msg())
	assert.False(t, serverStream.Receive())
	verifyHeaders(t, serverStream.ResponseHeader())
	verifyHeaders(t, serverStream.ResponseTrailer())
}

func TestBidiStreamForClient_NoPanics(t *testing.T) {
	t.Parallel()
	initErr := errors.New("client init failure")
	bidiStream := &BidiStreamForClient[pingv1.CumSumRequest, pingv1.CumSumResponse]{err: initErr}
	res, err := bidiStream.Receive()
	assert.Nil(t, res)
	assert.ErrorIs(t, err, initErr)
	verifyHeaders(t, bidiStream.RequestHeader())
	verifyHeaders(t, bidiStream.ResponseHeader())
	verifyHeaders(t, bidiStream.ResponseTrailer())
	assert.ErrorIs(t, bidiStream.Send(&pingv1.CumSumRequest{}), initErr)
	assert.ErrorIs(t, bidiStream.CloseReceive(), initErr)
	assert.ErrorIs(t, bidiStream.CloseSend(), initErr)
}

func verifyHeaders(t *testing.T, headers http.Header) {
	t.Helper()
	assert.Equal(t, headers, http.Header{})

	// Verify set/del don't panic
	headers.Set("a", "b")
	headers.Del("a")
}
