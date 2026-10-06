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
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// frameReader reads a connection on its own goroutine, one message ahead of
// its consumer.
//
// The WebSocket library processes control frames only inside a read, so a
// connection read only when the caller receives would leave Pongs unread and
// its peer indistinguishable from a dead one. Reading ahead by exactly one
// message keeps the socket drained of control frames without giving up
// backpressure: once a message is in hand, the goroutine waits for it to be
// taken before reading more.
//
// It also records when this end last heard from the peer, which is what
// keep-alive judges liveness by.
type frameReader struct {
	read    func(context.Context) (websocket.MessageType, []byte, error)
	results chan frameResult
	stopped chan struct{}
	stop    func()
	done    chan struct{}
	// ended is closed as soon as a read fails for any reason but the size
	// limit, which is the earliest this end can know the peer has gone.
	ended     chan struct{}
	closeEnds func()

	// waitingSince is when the goroutine began waiting on the socket, or zero
	// while it holds a message nobody has taken. Silence counts only while it
	// is waiting: a backlog is this end's delay, not the peer's.
	waitingSince atomic.Int64
	lastHeard    atomic.Int64
}

// errFrameReaderStopped is what a read after the connection ended returns,
// once the error that ended it has been delivered.
var errFrameReaderStopped = errors.New("websocket reader stopped")

type frameResult struct {
	messageType websocket.MessageType
	frame       []byte
	err         error
}

// newFrameReader starts reading with read, which runs under ctx. The goroutine
// ends after the first failed read, when ctx is done, or when stop is called.
func newFrameReader(
	ctx context.Context,
	read func(context.Context) (websocket.MessageType, []byte, error),
) *frameReader {
	stopped := make(chan struct{})
	ended := make(chan struct{})
	reader := &frameReader{
		read:      read,
		results:   make(chan frameResult),
		stopped:   stopped,
		stop:      sync.OnceFunc(func() { close(stopped) }),
		done:      make(chan struct{}),
		ended:     ended,
		closeEnds: sync.OnceFunc(func() { close(ended) }),
	}
	reader.lastHeard.Store(time.Now().UnixNano())
	go reader.run(ctx)
	return reader
}

// next returns the next message, or the error that ended the connection. A
// canceled ctx returns ctx.Err without waiting for the read in flight.
func (r *frameReader) next(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case result, ok := <-r.results:
		if !ok {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, nil, ctxErr
			}
			return 0, nil, errFrameReaderStopped
		}
		return result.messageType, result.frame, result.err
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

// close stops the goroutine and waits for it. The connection must already be
// closed, or the read in flight will not return.
func (r *frameReader) close() {
	r.stop()
	<-r.done
}

// connectionEnded reports whether a read has failed because the connection
// ended. A write that just failed may have raced the read that notices, so
// the caller may wait up to grace for it.
func (r *frameReader) connectionEnded(grace time.Duration) bool {
	if r == nil {
		return false
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-r.ended:
		return true
	case <-timer.C:
		return false
	}
}

// heard records a sign of life that did not arrive as a message: a Pong.
func (r *frameReader) heard() {
	r.lastHeard.Store(time.Now().UnixNano())
}

// silence reports how long the goroutine has waited on the socket without
// hearing from the peer. It is zero while a message is waiting to be taken.
func (r *frameReader) silence(now time.Time) time.Duration {
	waitingSince := r.waitingSince.Load()
	if waitingSince == 0 {
		return 0
	}
	return now.Sub(time.Unix(0, max(waitingSince, r.lastHeard.Load())))
}

func (r *frameReader) run(ctx context.Context) {
	defer close(r.done)
	defer close(r.results)
	for {
		r.waitingSince.Store(time.Now().UnixNano())
		messageType, frame, err := r.read(ctx)
		r.waitingSince.Store(0)
		if err == nil {
			r.heard()
		} else if !errors.Is(err, errMessageTooBig) {
			r.closeEnds()
		}
		select {
		case r.results <- frameResult{messageType: messageType, frame: frame, err: err}:
		case <-r.stopped:
			return
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}
