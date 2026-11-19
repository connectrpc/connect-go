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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/coder/websocket"
)

// wsStatusClientProtocolError is the code §13 has a client send for most faults.
// A browser script may only pass 1000 or 3000-4999 to close(), so the reason,
// not the code, says what went wrong.
const wsStatusClientProtocolError = websocket.StatusCode(3102)

// wsClientCall owns the lazy dial and is the [messageSender] the
// writer sends through. Dialing on first use rather than at construction is
// what lets a stream be created without a round trip.
type wsClientCall struct {
	config      *dialConfig
	dialOptions *websocket.DialOptions
	url         *url.URL
	// codec starts as the send codec and becomes the server's choice once the
	// handshake completes.
	codec connect.Codec
	// drain caps what the closing handshake reads; see drainLimiter.
	drain *drainLimiter

	// heartbeat carries the server's PINGs and PONGs to reader; the dial
	// options' callbacks hold it.
	heartbeat *heartbeat

	// stopCloseOnDone cancels the Close frame sent when ctx ends, once the
	// connection is being closed anyway.
	stopCloseOnDone func() bool

	// dialOnce also makes concurrent callers wait for the dial in progress.
	dialOnce sync.Once
	wsConn   *websocket.Conn
	reader   *frameReader
	keeper   *keepAlive
	response *http.Response
	dialErr  *connect.Error
}

// ensureDialed dials under ctx, the RPC's context, on first use. Later callers
// share the first one's result, so every caller must pass that same context.
func (c *wsClientCall) ensureDialed(ctx context.Context) *connect.Error {
	c.dialOnce.Do(func() {
		conn, err := c.handshake(ctx)
		if err != nil {
			c.dialErr = err
			return
		}
		c.start(ctx, conn)
	})
	return c.dialErr
}

// start begins reading and keeping alive an accepted connection.
func (c *wsClientCall) start(ctx context.Context, conn *websocket.Conn) {
	// The limit is enforced in readBoundedMessage, as on the server, so an
	// oversized message is classified and closed like any other fault.
	conn.SetReadLimit(-1)
	c.wsConn = conn
	// The library tears a connection down without a Close frame when a
	// read's or write's context ends, so the connection runs detached from
	// the call. §11.1 and §13 have a client whose call ends before S close
	// with 1000, which the server takes as the cancellation (§8).
	readLimit := c.config.readLimit
	connCtx := context.WithoutCancel(ctx)
	c.stopCloseOnDone = context.AfterFunc(ctx, func() {
		_ = closeWithDrain(conn, c.drain, websocket.StatusNormalClosure, "")
	})
	c.reader = newFrameReader(
		connCtx,
		func(readCtx context.Context) (websocket.MessageType, []byte, error) {
			return readBoundedMessage(readCtx, conn, readLimit, c.drain)
		},
		nil,
	)
	c.heartbeat.attach(c.reader)
	c.keeper = startKeepAlive(
		connCtx, conn, c.reader, c.config.keepAliveInterval, c.config.keepAliveTimeout,
		wsStatusClientProtocolError,
	)
}

// preventDial makes sure this call never dials, and reports whether it already
// had. A dial in progress is waited for.
func (c *wsClientCall) preventDial() bool {
	c.dialOnce.Do(func() {
		c.dialErr = errorf(connect.CodeCanceled, "stream closed before it was used")
	})
	return c.wsConn != nil
}

// connected reports whether the call dialed successfully. It is meaningful
// only after ensureDialed or preventDial has returned.
func (c *wsClientCall) connected() bool {
	return c.wsConn != nil
}

// negotiatedCodec is the codec the server chose, or the send codec until the
// handshake completes.
func (c *wsClientCall) negotiatedCodec() connect.Codec {
	return c.codec
}

// handshakeHeader returns the upgrade response's headers, or nil when there
// was no response.
func (c *wsClientCall) handshakeHeader() http.Header {
	if c.response == nil {
		return nil
	}
	return c.response.Header
}

// nextFrame reads the next frame from a dialed connection.
func (c *wsClientCall) nextFrame(ctx context.Context) (websocket.MessageType, []byte, error) {
	return c.reader.next(ctx)
}

// keepAliveExpired reports whether the server went silent for longer than the
// keep-alive timeout, and what that timeout is.
func (c *wsClientCall) keepAliveExpired() (time.Duration, bool) {
	// timedOut tolerates the nil keeper that disabled keep-alives leave; timeout
	// does not.
	if !c.keeper.timedOut() {
		return 0, false
	}
	return c.keeper.timeout(), true
}

// closeWith stops the call's background work and closes the connection with
// code and reason. A call that never dialed has nothing to close.
func (c *wsClientCall) closeWith(code websocket.StatusCode, reason string) error {
	if c.wsConn == nil {
		return nil
	}
	if c.stopCloseOnDone != nil {
		c.stopCloseOnDone()
	}
	c.keeper.stop()
	// Deferred so it runs once the close below has ended the read in flight.
	defer c.reader.close()
	return closeWithDrain(c.wsConn, c.drain, code, reason)
}

// send implements [messageSender], writing one message as a single WebSocket
// frame.
func (c *wsClientCall) send(ctx context.Context, text bool, data []byte) (int64, error) {
	if err := c.ensureDialed(ctx); err != nil {
		return 0, err
	}
	// Checked here and the write detached: a write under a context that ends
	// would drop the connection without the Close frame §13 asks for.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	written, err := writeFrame(context.WithoutCancel(ctx), c.wsConn, text, data)
	if err != nil && ctx.Err() == nil {
		// A server may end the RPC before reading every request, so a failed
		// write is how this end learns the stream is over. As over HTTP, it
		// reads as io.EOF and the caller calls Receive for the server's verdict.
		return written, fmt.Errorf("%w: %w", io.EOF, err)
	}
	return written, err
}

// dialConfig holds what every call from one transport dials with. It is built
// once and never written after, so calls share it without locking.
type dialConfig struct {
	// offered lists the tokens to offer, most preferred first: the send
	// codec's, then every other registered codec's in registration order.
	offered           []string
	codecs            registry[connect.Codec]
	handshakeTimeout  time.Duration
	readLimit         int64
	keepAliveInterval time.Duration
	keepAliveTimeout  time.Duration
}

// selectedSubprotocol maps the echoed token back to the one this client
// offered, or "" when the server echoed none. Token matching is
// case-insensitive, as the library's own check is.
func (d *dialConfig) selectedSubprotocol(echoed string) string {
	for _, token := range d.offered {
		if echoed != "" && strings.EqualFold(token, echoed) {
			return token
		}
	}
	return ""
}
