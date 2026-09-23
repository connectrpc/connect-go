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

package connectwebsocket_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

// scriptedServer sends a fixed number of messages and then either ends the
// stream or fails, so a test can place the error before, after, or instead of
// the messages. Every RPC sets a trailer: the end-of-stream message carries it, and
// a stream that ends the wrong way loses it.
type scriptedServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	messages int   // messages to send before finishing
	err      error // returned after those messages; nil ends the stream cleanly
}

func (s scriptedServer) setTrailer(ctx context.Context) {
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		info.ResponseTrailer().Set(trailerKey, "set-by-handler")
	}
}

func (s scriptedServer) CountUp(
	ctx context.Context,
	_ *pingv1.CountUpRequest,
	stream pingv1connect.PingServiceCountUpServerStream,
) error {
	s.setTrailer(ctx)
	for i := range s.messages {
		if err := stream.Send(&pingv1.CountUpResponse{Number: int64(i)}); err != nil {
			return err
		}
	}
	return s.err
}

func (s scriptedServer) CumSum(
	ctx context.Context,
	stream pingv1connect.PingServiceCumSumServerStream,
) error {
	s.setTrailer(ctx)
	var sent int
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return s.err
		}
		if err != nil {
			return err
		}
		if sent == s.messages {
			return s.err
		}
		if err := stream.Send(&pingv1.CumSumResponse{Sum: req.Number}); err != nil {
			return err
		}
		sent++
	}
}

func (s scriptedServer) Sum(
	ctx context.Context,
	stream pingv1connect.PingServiceSumServerStream,
) (*pingv1.SumResponse, error) {
	s.setTrailer(ctx)
	var total int64
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		total += req.Number
	}
	if s.err != nil {
		return nil, s.err
	}
	return &pingv1.SumResponse{Sum: total}, nil
}

func newScriptedClient(tb testing.TB, handler scriptedServer) pingv1connect.PingServiceClient {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, handler)
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)

	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
		connectwebsocket.WithSelector(connectwebsocket.SelectAll),
	)
	assert.Nil(tb, err)
	return pingv1connect.NewPingServiceClient(connect.NewClient(transport))
}

// A server stream that sends nothing still has to terminate: the client's very
// first Receive is the one that must report io.EOF, with the trailers intact.
func TestServerStreamEndsWithNoMessages(t *testing.T) {
	t.Parallel()
	client := newScriptedClient(t, scriptedServer{})
	ctx, info := connect.NewClientContext(t.Context())
	stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 0})
	assert.Nil(t, err)
	_, err = stream.Receive()
	assert.True(t, errors.Is(err, io.EOF))
	assert.Nil(t, stream.Close())
	assert.Equal(t, info.ResponseTrailer().Get(trailerKey), "set-by-handler")
}

// Neither side sends a message: the client closes its half immediately and the
// handler returns on the resulting EOF.
func TestBidiEndsWithNoMessagesEitherWay(t *testing.T) {
	t.Parallel()
	client := newScriptedClient(t, scriptedServer{})
	ctx, info := connect.NewClientContext(t.Context())
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	assert.Nil(t, stream.CloseSend())
	_, err = stream.Receive()
	assert.True(t, errors.Is(err, io.EOF))
	assert.Nil(t, stream.Close())
	assert.Equal(t, info.ResponseTrailer().Get(trailerKey), "set-by-handler")
}

// A client stream with no messages: the request side opens and closes without
// a single message, and the handler still answers.
func TestClientStreamEndsWithNoMessages(t *testing.T) {
	t.Parallel()
	client := newScriptedClient(t, scriptedServer{})
	ctx, info := connect.NewClientContext(t.Context())
	stream, err := client.Sum(ctx)
	assert.Nil(t, err)
	res, err := stream.CloseAndReceive()
	assert.Nil(t, err)
	assert.Equal(t, res.Sum, int64(0))
	assert.Equal(t, info.ResponseTrailer().Get(trailerKey), "set-by-handler")
}

// An error with no messages ahead of it must arrive as the error, not as EOF:
// the two are a single message apart on the wire.
func TestServerStreamErrorsBeforeAnyMessage(t *testing.T) {
	t.Parallel()
	client := newScriptedClient(t, scriptedServer{
		err: connect.NewError(connect.CodeResourceExhausted, "nothing to give"),
	})
	ctx, info := connect.NewClientContext(t.Context())
	stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 0})
	assert.Nil(t, err)
	_, err = stream.Receive()
	assert.NotNil(t, err)
	assert.True(t, !errors.Is(err, io.EOF))
	assert.Equal(t, connect.CodeOf(err), connect.CodeResourceExhausted)
	assert.Equal(t, connect.CodeOf(err).String(), "resource_exhausted")
	assert.Nil(t, stream.Close())
	assert.Equal(t, info.ResponseTrailer().Get(trailerKey), "set-by-handler")
}

// Messages already delivered must survive the failure that follows them: a
// caller reads all three, and only then hears why the stream stopped.
func TestServerStreamErrorsAfterMessages(t *testing.T) {
	t.Parallel()
	client := newScriptedClient(t, scriptedServer{
		messages: 3,
		err:      connect.NewError(connect.CodeAborted, "gave up partway"),
	})
	ctx, info := connect.NewClientContext(t.Context())
	stream, err := client.CountUp(ctx, &pingv1.CountUpRequest{Number: 3})
	assert.Nil(t, err)
	for i := range 3 {
		res, err := stream.Receive()
		assert.Nil(t, err)
		assert.Equal(t, res.Number, int64(i))
	}
	_, err = stream.Receive()
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeAborted)
	assert.True(t, strings.Contains(err.Error(), "gave up partway"))
	assert.Nil(t, stream.Close())
	assert.Equal(t, info.ResponseTrailer().Get(trailerKey), "set-by-handler")
}

// The bidi shape of the same thing, with traffic in both directions before the
// handler fails: the client's send side is still open when the error arrives.
func TestBidiErrorsAfterMessagesBothWays(t *testing.T) {
	t.Parallel()
	client := newScriptedClient(t, scriptedServer{
		messages: 2,
		err:      connect.NewError(connect.CodeInternal, "stopped after two"),
	})
	ctx, info := connect.NewClientContext(t.Context())
	stream, err := client.CumSum(ctx)
	assert.Nil(t, err)
	for i := range 2 {
		assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: int64(i + 1)}))
		res, err := stream.Receive()
		assert.Nil(t, err)
		assert.Equal(t, res.Sum, int64(i+1))
	}
	// The third send is what trips the handler into returning its error. The
	// send itself may succeed: it only has to reach the socket.
	_ = stream.Send(&pingv1.CumSumRequest{Number: 3})
	_, err = stream.Receive()
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
	assert.Nil(t, stream.Close())
	assert.Equal(t, info.ResponseTrailer().Get(trailerKey), "set-by-handler")
}

// A client stream that fails has no response message at all, so the error has
// to surface from CloseAndReceive rather than from a message read.
func TestClientStreamErrorsWithNoResponse(t *testing.T) {
	t.Parallel()
	client := newScriptedClient(t, scriptedServer{
		err: connect.NewError(connect.CodeInvalidArgument, "bad sum"),
	})
	ctx, info := connect.NewClientContext(t.Context())
	stream, err := client.Sum(ctx)
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 1}))
	_, err = stream.CloseAndReceive()
	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument)
	assert.Equal(t, info.ResponseTrailer().Get(trailerKey), "set-by-handler")
}
