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

package wstransport_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const handlerErrorCaseHeader = "X-Error-Case"

// handlerErrorServer fails every RPC with the error named by the request's
// X-Error-Case header.
type handlerErrorServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	errorsByCase map[string]error
}

func (s *handlerErrorServer) errorFor(ctx context.Context) error {
	info, ok := connect.CallInfoForServerContext(ctx)
	if !ok {
		return connect.NewError(connect.CodeInternal, "no call info")
	}
	return s.errorsByCase[info.RequestHeader().Get(handlerErrorCaseHeader)]
}

func (s *handlerErrorServer) Ping(ctx context.Context, _ *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	return nil, s.errorFor(ctx)
}

func (s *handlerErrorServer) CountUp(
	ctx context.Context,
	_ *pingv1.CountUpRequest,
	_ pingv1connect.PingServiceCountUpServerStream,
) error {
	return s.errorFor(ctx)
}

// A handler's error is scrubbed before it reaches the wire, as it is over
// HTTP: only a locally authored *connect.Error keeps its message and details.
func TestHandlerErrorIsScrubbed(t *testing.T) {
	t.Parallel()
	detail, detailErr := connectproto.NewErrorDetail(&wrapperspb.StringValue{Value: "upstream detail"})
	assert.Nil(t, detailErr)
	remoteErr := connect.NewError(connect.CodePermissionDenied, "upstream secret").
		WithDetail(detail).
		WithRemote()
	localErr := connect.NewError(connect.CodeInvalidArgument, "bad number").WithDetail(detail)
	server := &handlerErrorServer{errorsByCase: map[string]error{
		"plain":    errors.New("disk full: /var/data"),
		"wrapped":  fmt.Errorf("query users: %w", errors.New("pq: connection refused")),
		"canceled": fmt.Errorf("copy data: %w", context.Canceled),
		"deadline": fmt.Errorf("copy data: %w", context.DeadlineExceeded),
		// A handler returning a nil *connect.Error as error boxes a typed nil.
		"typed_nil": (*connect.Error)(nil),
		"remote":    remoteErr,
		"local":     localErr,
	}}
	testCases := []struct {
		name        string
		wantCode    connect.Code
		wantMessage string
		wantDetails int
	}{
		{name: "plain", wantCode: connect.CodeUnknown},
		{name: "wrapped", wantCode: connect.CodeUnknown},
		{name: "canceled", wantCode: connect.CodeCanceled},
		{name: "deadline", wantCode: connect.CodeDeadlineExceeded},
		{name: "typed_nil", wantCode: connect.CodeUnknown},
		{name: "remote", wantCode: connect.CodeInternal},
		{name: "local", wantCode: connect.CodeInvalidArgument, wantMessage: "bad number", wantDetails: 1},
	}
	httpServer := newServerFor(t, server)
	transport := connecthttp.NewTransport(
		httpServer.Client(),
		httpServer.URL,
		connecthttp.WithWebSocket(connecthttp.SelectAll),
	)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	assertError := func(t *testing.T, err error, wantCode connect.Code, wantMessage string, wantDetails int) {
		t.Helper()
		connectErr, ok := errors.AsType[*connect.Error](err)
		if !assert.True(t, ok) {
			return
		}
		assert.Equal(t, connectErr.Code(), wantCode)
		assert.Equal(t, connectErr.Message(), wantMessage)
		assert.Equal(t, len(connectErr.Details()), wantDetails)
	}
	for _, testCase := range testCases {
		t.Run(testCase.name+"/unary", func(t *testing.T) {
			t.Parallel()
			ctx, info := connect.NewClientContext(t.Context())
			info.RequestHeader().Set(handlerErrorCaseHeader, testCase.name)
			_, err := client.Ping(ctx, &pingv1.PingRequest{})
			assertError(t, err, testCase.wantCode, testCase.wantMessage, testCase.wantDetails)
		})
		t.Run(testCase.name+"/server_stream", func(t *testing.T) {
			t.Parallel()
			ctx, info := connect.NewClientContext(t.Context())
			info.RequestHeader().Set(handlerErrorCaseHeader, testCase.name)
			stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 1})
			assert.Nil(t, err)
			_, err = stream.Receive()
			assertError(t, err, testCase.wantCode, testCase.wantMessage, testCase.wantDetails)
			assert.Nil(t, stream.Close())
		})
	}
}
