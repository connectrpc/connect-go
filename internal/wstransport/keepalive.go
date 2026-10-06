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
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// keepAliveMisses is how many intervals of silence condemn a peer. One would
// hang up on a peer whose Pong was merely slow; two is what SignalR and
// Socket.IO settle on.
const keepAliveMisses = 2

// keepAlive pings the peer on a fixed interval, so that a proxy which drops
// idle connections sees traffic, and hangs up on a peer that has been silent
// for keepAliveMisses intervals.
//
// Silence is judged by the frameReader: any message or Pong is a sign of
// life, and time spent holding a message this end has not yet taken does not
// count against the peer.
//
// Every method is a no-op on a nil keepAlive, which is what a disabled one is.
type keepAlive struct {
	conn      *websocket.Conn
	reader    *frameReader
	interval  time.Duration
	closeCode websocket.StatusCode
	cancel    context.CancelFunc
	done      chan struct{}
	pings     sync.WaitGroup
	// expired is set before the connection is closed, so the read that fails
	// as a result can say why.
	expired atomic.Bool
}

// startKeepAlive returns nil when interval is not positive. closeCode is what
// this side sends a silent peer: 1011 from a server, 3111 from a client (§13).
// The pinging ends when ctx is done, when stop is called, or when the
// connection closes.
func startKeepAlive(
	ctx context.Context,
	conn *websocket.Conn,
	reader *frameReader,
	interval time.Duration,
	closeCode websocket.StatusCode,
) *keepAlive {
	if interval <= 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	keeper := &keepAlive{
		conn:      conn,
		reader:    reader,
		interval:  interval,
		closeCode: closeCode,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	go keeper.run(ctx)
	return keeper
}

// stop ends the pinging and waits for it, so that nothing writes to the
// connection once its owner has let go of it.
func (k *keepAlive) stop() {
	if k == nil {
		return
	}
	k.cancel()
	<-k.done
}

// timedOut reports that this end hung up because the peer went silent.
func (k *keepAlive) timedOut() bool {
	return k != nil && k.expired.Load()
}

// timeout is how long a peer may stay silent.
func (k *keepAlive) timeout() time.Duration {
	return keepAliveMisses * k.interval
}

func (k *keepAlive) run(ctx context.Context) {
	defer close(k.done)
	defer k.pings.Wait()
	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	// A late tick can find the timeout already passed. Requiring a Ping an
	// interval old means no peer is hung up on before it could have answered.
	var lastPing time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			pinged := !lastPing.IsZero() && now.Sub(lastPing) >= k.interval
			if pinged && k.reader.silence(now) >= k.timeout() {
				k.expired.Store(true)
				_ = k.conn.Close(k.closeCode, "keep-alive timeout")
				return
			}
			lastPing = now
		}
		k.pings.Go(func() { k.ping(ctx) })
	}
}

// ping sends one Ping and records its Pong. It runs apart from the ticker, and
// waits as long as the peer may stay silent, so that a Pong slower than one
// interval still counts. A missing one is judged by silence rather than here,
// because a peer that sent a message has answered too.
func (k *keepAlive) ping(ctx context.Context) {
	pingCtx, cancel := context.WithTimeout(ctx, k.timeout())
	defer cancel()
	if k.conn.Ping(pingCtx) == nil {
		k.reader.heard()
	}
}
