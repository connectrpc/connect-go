// Copyright 2021-2024 The Connect Authors
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
	"context"
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/memhttp/memhttptest"
)

var ErrOKToIgnore = errors.New("some error that is ok to ignore")

func TestClientErrorHandling(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("/connect.ping.v1.PingService/Ping", NewUnaryHandler(
		"/connect.ping.v1.PingService/Ping",
		func(ctx context.Context, r *Request[pingv1.PingRequest]) (*Response[pingv1.PingResponse], error) {
			return nil, NewError(CodeCanceled, ErrOKToIgnore)
		},
	))
	server := memhttptest.NewServer(t, mux)

	client := NewClient[pingv1.PingRequest, pingv1.PingResponse](
		server.Client(),
		server.URL()+"/connect.ping.v1.PingService/Ping",
		WithInterceptors(
			errorHidingInterceptor{},
		),
	)
	ctx := context.Background()

	_, err := client.CallUnary(ctx, NewRequest[pingv1.PingRequest](nil))
	assert.Nil(t, err)
}

// errorHidingInterceptor is a simple interceptor that hides errors based on
// some criteria (in this case, if the error is a CodeCanceled error). It is
// use to reproduce an issue with error handling in the client, where the
// type information is lost between unaryFunc and client.callUnary.
type errorHidingInterceptor struct{}

func (e errorHidingInterceptor) WrapStreamingClient(StreamingClientFunc) StreamingClientFunc {
	panic("unimplemented")
}

func (e errorHidingInterceptor) WrapStreamingHandler(StreamingHandlerFunc) StreamingHandlerFunc {
	panic("unimplemented")
}

func (e errorHidingInterceptor) WrapUnary(next UnaryFunc) UnaryFunc {
	return func(ctx context.Context, req AnyRequest) (_ AnyResponse, retErr error) {
		res, err := next(ctx, req)
		if CodeOf(err) == CodeCanceled { // some criteria for ignored errors
			return res, nil
		}
		return res, err
	}
}
