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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"github.com/coder/websocket"
)

// wsTestReadTimeout bounds a raw-wire read: a backstop against a hang, not a
// latency assertion. The external test package has its own; the two packages
// cannot share one.
const wsTestReadTimeout = 10 * time.Second

// deadSender fails every write, standing in for a socket whose peer has gone.
// A real disconnect cannot be provoked reliably — whether a write lands in a
// buffer or meets a reset is a matter of timing — so the condition is supplied
// directly here.
type deadSender struct{}

func (deadSender) reason() string {
	return "write tcp 127.0.0.1:8080->127.0.0.1:1234: write: broken pipe"
}

func (d deadSender) send(_ bool, _ []byte) (int64, error) {
	return 0, errors.New(d.reason())
}

// A verdict that cannot be delivered because the client already left is not a
// server error. Reporting it would log one for every abandoned subscription.
func TestCloseSwallowsTheWriteErrorWhenThePeerIsGone(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		sawEnd      bool
		eof         bool
		wantSwallow bool
	}{
		{
			name:        "peer left without C",
			eof:         true,
			wantSwallow: true,
		},
		{
			name:   "peer sent C and is waiting for the verdict",
			eof:    true,
			sawEnd: true,
		},
		{
			name: "read path never reached the end, so nothing says the peer left",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			conn := &websocketHandlerConn{
				unmarshaler: websocketUnmarshaler{
					eof:                  test.eof,
					sawEndOfClientStream: test.sawEnd,
				},
			}
			writeErr := errorf(connect.CodeUnknown, "write message: %s", deadSender{}.reason())
			got := conn.terminalWriteError(writeErr)
			if test.wantSwallow {
				assert.Nil(t, got)
				return
			}
			assert.NotNil(t, got)
			assert.Equal(t, got.Code(), connect.CodeUnknown)
		})
	}
}

// connPair returns the two ends of a live WebSocket, so a test can drive one
// side through the handler's own types and observe the other as a peer does.
func connPair(tb testing.TB) (server, client *websocket.Conn) {
	tb.Helper()
	accepted := make(chan *websocket.Conn, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(
		func(responseWriter http.ResponseWriter, request *http.Request) {
			conn, err := websocket.Accept(responseWriter, request, nil)
			if err != nil {
				return
			}
			accepted <- conn
			<-request.Context().Done()
		},
	))
	tb.Cleanup(httpServer.Close)

	client, response, err := websocket.Dial(tb.Context(),
		"ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	assert.Nil(tb, err)
	tb.Cleanup(func() { _ = client.CloseNow() })
	return <-accepted, client
}

// A peer that is still listening must be told why it will get no verdict. The
// S message could not be written, so the close frame is the only channel left,
// and a bare 1000 would read as an orderly finish.
func TestCloseReportsAFailedVerdictToAPresentPeer(t *testing.T) {
	t.Parallel()
	serverConn, clientConn := connPair(t)
	conn := &websocketHandlerConn{
		wsConn: serverConn,
		marshaler: messageWriter{
			ctx:    t.Context(),
			sender: deadSender{},
		},
		// The peer is still there: it never sent C, and the read path never
		// reached EOF, so the failed write is not excused.
		leadingSent:     true,
		responseTrailer: http.Header{},
	}

	// Close runs the closing handshake, which waits for the peer's own close
	// frame — the read below — so it cannot be called inline.
	closed := make(chan error, 1)
	go func() { closed <- conn.Close(nil) }()

	// Bounded: an unbounded read here would wait out the package's own timeout
	// if the close frame never arrived, rather than failing this one test.
	readCtx, cancelRead := context.WithTimeout(t.Context(), wsTestReadTimeout)
	defer cancelRead()
	_, _, readErr := clientConn.Read(readCtx)
	assert.NotNil(t, readErr)
	assert.Equal(t, websocket.CloseStatus(readErr), websocket.StatusInternalError)
	var closeErr websocket.CloseError
	assert.True(t, errors.As(readErr, &closeErr))
	assert.True(t, strings.Contains(closeErr.Reason, "broken pipe"))

	returned := <-closed
	assert.NotNil(t, returned)
	assert.True(t, strings.Contains(returned.Error(), "broken pipe"))
}

// A close reason is whatever the failure said, and RFC 6455 caps a control
// payload at 125 bytes with two spent on the status code. The library rejects
// an over-long reason rather than trimming it, so a reason that does not fit
// means no close frame at all — the peer learns nothing.
func TestCloseReasonFitsTheFrame(t *testing.T) {
	t.Parallel()
	const limit = 123
	for _, test := range []struct {
		name   string
		reason string
		want   int
	}{
		{name: "short", reason: strings.Repeat("x", 1), want: 1},
		{name: "at the limit", reason: strings.Repeat("x", limit), want: limit},
		{name: "one past the limit", reason: strings.Repeat("x", limit+1), want: limit},
		{name: "far past the limit", reason: strings.Repeat("x", 4096), want: limit},
		{
			// The cut lands one byte into a three-byte rune, so it has to back
			// off past the whole rune: a close reason must be valid UTF-8.
			name:   "cut inside a multi-byte rune",
			reason: strings.Repeat("x", limit-1) + "\u2603",
			want:   limit - 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := closeReason(test.reason)
			assert.Equal(t, len(got), test.want)
			assert.True(t, strings.HasPrefix(test.reason, got))
			assert.True(t, utf8.ValidString(got))

			// The contract is the library's, not arithmetic: a reason it
			// refuses to marshal is a close frame that never goes out.
			serverConn, clientConn := connPair(t)
			// Answer the close frame, so the closing handshake finishes
			// instead of waiting out its timeout.
			go func() { _, _, _ = clientConn.Read(context.Background()) }()
			err := serverConn.Close(websocket.StatusInternalError, got)
			assert.True(t, err == nil || !strings.Contains(err.Error(), "reason string max"))
		})
	}
}

// A body that cannot be written must fail the Send that wrote it. Swallowing it
// would let a handler run to completion believing a peer got messages it never
// did.
func TestSendReportsAFailedBodyWrite(t *testing.T) {
	t.Parallel()
	codecs, codecErr := newCodecPair([]connect.Codec{connectproto.NewBinaryCodec()})
	assert.Nil(t, codecErr)
	conn := &websocketHandlerConn{
		marshaler: messageWriter{
			ctx:    t.Context(),
			sender: deadSender{},
			codecs: codecs,
		},
		leadingSent: true, // isolate the body write from the metadata write
	}

	err := conn.Send(&pingv1.CumSumResponse{Sum: 1})
	assert.NotNil(t, err)
	assert.True(t, strings.Contains(err.Error(), "broken pipe"))
}

// pastDeadlineContext is the window the two clocks leave open: the deadline has
// passed, but the context's timer goroutine has not yet set Err. It is supplied
// directly because the real window is a few microseconds wide.
type pastDeadlineContext struct{ context.Context }

func (pastDeadlineContext) Deadline() (time.Time, bool) {
	return time.Now().Add(-time.Second), true
}

func (pastDeadlineContext) Err() error { return nil }

// A read that fails at or past the deadline is the deadline, whichever clock
// noticed first. Reporting Unavailable would invite a retry with no time left
// to run it.
func TestReadFailureAtTheDeadlineIsDeadlineExceeded(t *testing.T) {
	t.Parallel()
	serverConn, clientConn := connPair(t)
	_ = serverConn.CloseNow()
	// Closed before the read, so the failure needs no timing of its own.
	_ = clientConn.CloseNow()

	call := &wsClientCall{
		ctx:      pastDeadlineContext{Context: t.Context()},
		wsConn:   clientConn,
		dialDone: make(chan struct{}),
	}
	close(call.dialDone)
	call.dialOnce.Do(func() {}) // consume it, so ensureDialed will not dial
	call.reader = newFrameReader(call.ctx, clientConn.Read)
	t.Cleanup(call.reader.close)

	unmarshaler := &websocketClientUnmarshaler{call: call}
	err := unmarshaler.unmarshal(&pingv1.CumSumResponse{})

	assert.NotNil(t, err)
	assert.Equal(t, err.Code(), connect.CodeDeadlineExceeded)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
}
