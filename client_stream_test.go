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

	"github.com/bufbuild/connect-go/internal/assert"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
)

func TestClientStreamForClient_NoPanics(t *testing.T) {
	t.Parallel()
	initErr := errors.New("client init failure")
	cs := &ClientStreamForClient[pingv1.PingRequest, pingv1.PingResponse]{err: initErr}
	assert.ErrorIs(t, cs.Send(&pingv1.PingRequest{}), initErr)
	assert.Equal(t, len(cs.RequestHeader()), 0)
	res, err := cs.CloseAndReceive()
	assert.Nil(t, res)
	assert.ErrorIs(t, err, initErr)
}

func TestServerStreamForClient_NoPanics(t *testing.T) {
	t.Parallel()
	initErr := errors.New("client init failure")
	ss := &ServerStreamForClient[pingv1.PingResponse]{err: initErr}
	assert.ErrorIs(t, ss.Err(), initErr)
	assert.ErrorIs(t, ss.Close(), initErr)
	assert.NotNil(t, ss.Msg())
	assert.False(t, ss.Receive())
	assert.Equal(t, ss.ResponseHeader(), http.Header{})
	assert.Equal(t, ss.ResponseTrailer(), http.Header{})
}

func TestBidiStreamForClient_NoPanics(t *testing.T) {
	t.Parallel()
	initErr := errors.New("client init failure")
	bs := &BidiStreamForClient[pingv1.CumSumRequest, pingv1.CumSumResponse]{err: initErr}
	res, err := bs.Receive()
	assert.Nil(t, res)
	assert.ErrorIs(t, err, initErr)
	assert.Equal(t, bs.RequestHeader(), http.Header{})
	assert.Equal(t, bs.ResponseHeader(), http.Header{})
	assert.Equal(t, bs.ResponseTrailer(), http.Header{})
	assert.ErrorIs(t, bs.Send(&pingv1.CumSumRequest{}), initErr)
	assert.ErrorIs(t, bs.CloseReceive(), initErr)
	assert.ErrorIs(t, bs.CloseSend(), initErr)
}
