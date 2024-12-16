package connect_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect"
)

// See https://github.com/connectrpc/connect-go/pull/801 for further context
func TestClientStream_CancelContext(t *testing.T) {
	t.Run("HTTP2 disabled", func(t *testing.T) {
		t.Parallel()
		testClientStream_CancelContext(t, false)
	})
	t.Run("HTTP2 enabled", func(t *testing.T) {
		t.Parallel()
		testClientStream_CancelContext(t, true)
	})
}

func testClientStream_CancelContext(t *testing.T, enableHTTP2 bool) {
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(pingServer{
		delayCountUp: 3 * time.Second,
	}))

	s := httptest.NewUnstartedServer(mux)
	s.EnableHTTP2 = enableHTTP2
	s.StartTLS()

	client := pingv1connect.NewPingServiceClient(
		s.Client(),
		s.URL,
	)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.CountUp(ctx, connect.NewRequest(&pingv1.CountUpRequest{
		Number: 100,
	}))

	assert.Nil(t, err)

	msg := make(chan int64)
	go func() {
		for stream.Receive() {
			select {
			case msg <- stream.Msg().Number:
			default:
			}
		}
		close(msg)
	}()

	assert.Equal(t, <-msg, 1)

	closed := make(chan struct{})
	go func() {
		// close stream
		assert.Nil(t, stream.Close())
		close(closed)
	}()

	time.Sleep(10 * time.Millisecond) // delay to ensure that stream.Close has already been called
	cancel()

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Error("stream was not closed within 1s")
	}
	select {
	case _, ok := <-msg:
		assert.False(t, ok)
	case <-time.After(time.Second):
		t.Error("stream was not done receiving within 1s")
	}

	// ensure the connection gets closed quickly
	startClosing := time.Now()
	s.Close()
	assert.True(t, time.Since(startClosing) < time.Second, assert.Sprintf("server.Close took too long: %s", time.Since(startClosing)))
}
