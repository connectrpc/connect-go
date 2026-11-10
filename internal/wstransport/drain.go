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
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/coder/websocket"
)

// defaultDrainBudget bounds the closing drain of a side with no read limit.
// It matches the 4MiB message limit common to gRPC implementations.
const defaultDrainBudget = 4 << 20

var errDrainBudgetSpent = errors.New("closing drain reached its byte limit")

// drainLimiter caps how much of a connection is read once this side starts a
// closing handshake. The WebSocket library's handshake reads and discards the
// peer's frames until its Close frame arrives, bounded at five seconds but by
// no number of bytes, so a peer with a compression bomb queued could make the
// drain read all of it. §13 asks for a bound on bytes as well as time, and
// this supplies it.
//
// Every method is a no-op on a nil drainLimiter.
type drainLimiter struct {
	budget int64
	// remaining is negative until the drain begins.
	remaining atomic.Int64
}

// newDrainLimiter returns a limiter whose budget is readLimit, the size of
// the largest message this side would accept, so the drain costs no more
// than one message it would have read anyway. A readLimit that is not
// positive means no limit, and gets defaultDrainBudget.
func newDrainLimiter(readLimit int64) *drainLimiter {
	limiter := &drainLimiter{budget: readLimit}
	if readLimit <= 0 {
		limiter.budget = defaultDrainBudget
	}
	limiter.remaining.Store(-1)
	return limiter
}

// begin starts counting reads against the budget. Only the first call counts.
func (l *drainLimiter) begin() {
	if l == nil {
		return
	}
	l.remaining.CompareAndSwap(-1, l.budget)
}

func (l *drainLimiter) read(reader io.Reader, data []byte) (int, error) {
	if l == nil {
		return reader.Read(data)
	}
	remaining := l.remaining.Load()
	if remaining < 0 {
		return reader.Read(data)
	}
	if remaining == 0 {
		return 0, errDrainBudgetSpent
	}
	if int64(len(data)) > remaining {
		data = data[:remaining]
	}
	n, err := reader.Read(data)
	l.remaining.Add(-int64(n))
	return n, err
}

// closeWithDrain sends a Close frame with code and completes the closing
// handshake, reading at most the limiter's budget while it waits for the
// peer's own Close frame.
func closeWithDrain(conn *websocket.Conn, limiter *drainLimiter, code websocket.StatusCode, reason string) error {
	limiter.begin()
	return conn.Close(code, reason)
}

// drainConn is a hijacked server connection whose reads a drainLimiter caps.
type drainConn struct {
	net.Conn

	limiter *drainLimiter
}

func (c *drainConn) Read(data []byte) (int, error) {
	return c.limiter.read(c.Conn, data)
}

// drainResponseWriter hands the WebSocket library a drainConn when it
// hijacks the handshake's connection.
type drainResponseWriter struct {
	http.ResponseWriter

	limiter *drainLimiter
}

func (w *drainResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not implement http.Hijacker")
	}
	conn, readWriter, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	// The library reads what readWriter already buffered, then reads the
	// returned connection.
	return &drainConn{Conn: conn, limiter: w.limiter}, readWriter, nil
}

func (w *drainResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// drainBody is the body of a client's 101 response, which is the upgraded
// connection, with its reads capped by a drainLimiter.
type drainBody struct {
	io.ReadWriteCloser

	limiter *drainLimiter
}

func (b *drainBody) Read(data []byte) (int, error) {
	return b.limiter.read(b.ReadWriteCloser, data)
}

// drainRoundTripper caps the reads of the connection that a WebSocket
// handshake upgrades.
type drainRoundTripper struct {
	base    http.RoundTripper
	limiter *drainLimiter
}

func (t *drainRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		return response, err
	}
	if body, ok := response.Body.(io.ReadWriteCloser); ok {
		response.Body = &drainBody{ReadWriteCloser: body, limiter: t.limiter}
	}
	return response, nil
}

// withDrainLimiter returns a copy of client whose WebSocket handshakes yield
// connections that limiter caps.
func withDrainLimiter(client *http.Client, limiter *drainLimiter) *http.Client {
	limited := *client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	limited.Transport = &drainRoundTripper{base: base, limiter: limiter}
	return &limited
}
