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

package connect_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect"
)

func TestNewClient_InitFailure(t *testing.T) {
	t.Parallel()
	client := pingv1connect.NewPingServiceClient(
		http.DefaultClient,
		"http://127.0.0.1:8080",
		// This triggers an error during initialization, so each call will short circuit returning an error.
		connect.WithSendCompression("invalid"),
	)
	validateExpectedError := func(t *testing.T, err error) {
		t.Helper()
		assert.NotNil(t, err)
		var connectErr *connect.Error
		assert.True(t, errors.As(err, &connectErr))
		assert.Equal(t, connectErr.Message(), `unknown compression "invalid"`)
	}

	t.Run("unary", func(t *testing.T) {
		t.Parallel()
		_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{}))
		validateExpectedError(t, err)
	})

	t.Run("bidi", func(t *testing.T) {
		t.Parallel()
		bidiStream := client.CumSum(context.Background())
		err := bidiStream.Send(&pingv1.CumSumRequest{})
		validateExpectedError(t, err)
	})

	t.Run("client_stream", func(t *testing.T) {
		t.Parallel()
		clientStream := client.Sum(context.Background())
		err := clientStream.Send(&pingv1.SumRequest{})
		validateExpectedError(t, err)
	})

	t.Run("server_stream", func(t *testing.T) {
		t.Parallel()
		_, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{Number: 3}))
		validateExpectedError(t, err)
	})
}
