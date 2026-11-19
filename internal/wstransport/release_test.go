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
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

type countUpServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (countUpServer) CountUp(
	_ context.Context,
	request *pingv1.CountUpRequest,
	stream pingv1connect.PingServiceCountUpServerStream,
) error {
	for number := range request.Number {
		if err := stream.Send(&pingv1.CountUpResponse{Number: number + 1}); err != nil {
			return err
		}
	}
	return nil
}

// The ClientStream contract says reading to io.EOF releases the stream's
// resources, and generated server-streaming code relies on it: it never calls
// Close. The reader and keep-alive goroutines must end without one, even under
// a context that is never canceled.
func TestReadingToEndOfStreamReleasesGoroutines(t *testing.T) {
	t.Parallel()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, countUpServer{})
	mux := http.NewServeMux()
	mux.Handle(
		pingv1connect.PingServiceCountUpProcedure,
		Upgrade(server, http.NotFoundHandler()),
	)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	transport, err := NewTransport(
		httpServer.URL,
		WithHTTPClient(httpServer.Client()),
		WithKeepAlive(time.Hour),
	)
	assert.Nil(t, err)
	spec := connect.Spec{
		Procedure:  pingv1connect.PingServiceCountUpProcedure,
		StreamType: connect.StreamTypeServer,
	}
	connectStream, err := transport.NewClientStream(context.Background(), spec)
	assert.Nil(t, err)
	assert.Nil(t, connectStream.Send(&pingv1.CountUpRequest{Number: 3}))
	assert.Nil(t, connectStream.CloseSend())
	for {
		receiveErr := connectStream.Receive(&pingv1.CountUpResponse{})
		if receiveErr != nil {
			assert.True(t, errors.Is(receiveErr, io.EOF))
			break
		}
	}

	stream, ok := connectStream.(*clientStream)
	assert.True(t, ok)
	call := stream.conn.call
	assert.NotNil(t, call.keeper)
	for name, done := range map[string]chan struct{}{
		"reader":     call.reader.done,
		"keep-alive": call.keeper.done,
	} {
		select {
		case <-done:
		case <-time.After(wsTestReadTimeout):
			t.Fatalf("%s goroutine still running after the stream was read to io.EOF", name)
		}
	}
}

// Close publishes the response metadata while Receive may still be merging it,
// which the ClientStream contract allows. Run under -race, which is how make
// shorttest runs it; the two goroutines share no other synchronization.
func TestResponseMetadataIsSafeDuringReceive(t *testing.T) {
	t.Parallel()
	call := &wsClientCall{}
	call.preventDial()
	conn := &websocketClientConn{
		ctx:             t.Context(),
		call:            call,
		trailer:         http.Header{"Trail": {"value"}},
		responseHeader:  make(http.Header),
		responseTrailer: make(http.Header),
	}
	receiveDone := make(chan struct{})
	go func() {
		defer close(receiveDone)
		assert.Nil(t, conn.mergeLeadingMetadata([]byte(`[["lead","value"]]`)))
		conn.endStreamSeen.Store(true)
		_ = conn.Receive(nil)
	}()
	for key := range conn.ResponseHeader() {
		_ = key
	}
	for key := range conn.ResponseTrailer() {
		_ = key
	}
	<-receiveDone
	assert.Equal(t, conn.ResponseHeader().Get("Lead"), "value")
	assert.Equal(t, conn.ResponseTrailer().Get("Trail"), "value")
}
