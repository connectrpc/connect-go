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
	"io"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

// §8.1 and §5: a message after C is a protocol error, and it ends the stream
// whatever the RPC's shape. The handler ignores its context and answers late,
// so a server that only noticed once the handler returned would send that
// answer first.
func TestMessageAfterEndOfClientStreamEndsTheStream(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		procedure string
		request   proto.Message
	}{
		{name: "unary", procedure: pingv1connect.PingServicePingProcedure, request: &pingv1.PingRequest{Number: 1}},
		{
			name:      "server streaming",
			procedure: pingv1connect.PingServiceCountUpProcedure,
			request:   &pingv1.CountUpRequest{Number: 1},
		},
		{name: "bidi", procedure: pingv1connect.PingServiceCumSumProcedure, request: &pingv1.CumSumRequest{Number: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer2(t, lateAnswerServer{delay: 300 * time.Millisecond})
			conn := dialRaw(t, httpServer, test.procedure)
			sendJSONMessage(t, conn, wireMetadata, []byte("[]"))
			sendProtoBody(t, conn, test.request)
			sendJSONMessage(t, conn, wireClientEndStream, nil)
			sendProtoBody(t, conn, test.request)

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			assert.True(t, strings.Contains(string(data), "invalid_argument"))
			ctx, cancel := readContext(t)
			defer cancel()
			_, _, err := conn.Read(ctx)
			assert.Equal(t, websocket.CloseStatus(err), websocket.StatusProtocolError)
		})
	}
}

// A unary or server-streaming call has exactly one request, which the server
// checks as Connect over HTTP does.
func TestSingleRequestCallsCheckTheRequestCount(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		requests int
		want     string
	}{
		{name: "no request", requests: 0, want: "zero messages"},
		{name: "two requests", requests: 2, want: "multiple messages"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := newHybridServer2(t, lateAnswerServer{})
			conn := dialRaw(t, httpServer, pingv1connect.PingServicePingProcedure)
			sendJSONMessage(t, conn, wireMetadata, []byte("[]"))
			for range test.requests {
				sendProtoBody(t, conn, &pingv1.PingRequest{Number: 1})
			}
			sendJSONMessage(t, conn, wireClientEndStream, nil)

			data := readServerOpening(t, conn)
			assert.Equal(t, data[0], wireServerEndStream)
			assert.True(t, strings.Contains(string(data), "unimplemented"))
			assert.True(t, strings.Contains(string(data), test.want))
		})
	}
}

// lateAnswerServer answers each request after delay, ignoring its context as a
// handler busy with slow work does.
type lateAnswerServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	delay time.Duration
}

func (s lateAnswerServer) Ping(_ context.Context, request *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	time.Sleep(s.delay)
	return &pingv1.PingResponse{Number: request.GetNumber()}, nil
}

func (s lateAnswerServer) CountUp(
	_ context.Context,
	request *pingv1.CountUpRequest,
	stream pingv1connect.PingServiceCountUpServerStream,
) error {
	time.Sleep(s.delay)
	return stream.Send(&pingv1.CountUpResponse{Number: request.GetNumber()})
}

func (s lateAnswerServer) CumSum(_ context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	var sum int64
	for {
		request, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		sum += request.GetNumber()
	}
	time.Sleep(s.delay)
	return stream.Send(&pingv1.CumSumResponse{Sum: sum})
}
