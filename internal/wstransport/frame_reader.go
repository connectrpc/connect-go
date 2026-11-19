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

// readAheadBytes is how much the reader buffers for a consumer that is not
// receiving. It is what lets the peer's C and Close frame, which queue behind
// any messages it sent, reach this end while a handler is busy (§8.1). Past
// it, the reader stops and backpressure reaches the peer through TCP.
const readAheadBytes = 64 << 10

// queuedMessageOverhead is charged per buffered message on top of its bytes,
// so that a stream of tiny messages cannot buffer an unbounded number of them.
const queuedMessageOverhead = 64

// frameReader reads a connection on its own goroutine, ahead of its consumer.
//
// The WebSocket library processes control frames only inside a read, so a
// connection read only when the caller receives would leave Pongs unread and
// its peer indistinguishable from a dead one. Reading ahead keeps the socket
// drained of control frames, and lets this end see the connection end while
// the consumer is busy elsewhere. Read-ahead is bounded by readAheadBytes, but
// one message is always read, whatever its size.
//
// It also records when this end last heard from the peer, which is what
// keep-alive judges liveness by.
type frameReader struct {
	read    func(context.Context) (websocket.MessageType, []byte, error)
	onEnded func()
	stopped chan struct{}
	stop    func()
	done    chan struct{}
	// ended is closed as soon as a read fails for any reason but the size
	// limit, which is the earliest this end can know the peer has gone.
	ended     chan struct{}
	closeEnds func()
	// taken wakes the goroutine when the consumer frees read-ahead room.
	taken chan struct{}

	mu          sync.Mutex
	queue       []frameResult
	queuedBytes int
	// finished is set once the goroutine will add nothing more to queue.
	finished bool
	// changed is closed and replaced whenever queue or finished changes.
	changed chan struct{}

	// waitingSince is when the goroutine began waiting on the socket, or zero
	// while read-ahead is full. Silence counts only while it is waiting: a
	// backlog is this end's delay, not the peer's.
	waitingSince atomic.Int64
	lastHeard    atomic.Int64
}

// heartbeat forwards the peer's PING and PONG frames to a frameReader as signs
// of life (§4.6). The WebSocket library takes its callbacks at the handshake,
// before the reader exists, so the reader is attached afterwards. Every method
// is a no-op on a nil heartbeat.
type heartbeat struct {
	reader atomic.Pointer[frameReader]
}

func (h *heartbeat) attach(reader *frameReader) {
	if h != nil {
		h.reader.Store(reader)
	}
}

func (h *heartbeat) heard() {
	if h == nil {
		return
	}
	if reader := h.reader.Load(); reader != nil {
		reader.heard()
	}
}

// onPing answers every PING, as RFC 6455 §5.5.2 requires.
func (h *heartbeat) onPing(context.Context, []byte) bool {
	h.heard()
	return true
}

func (h *heartbeat) onPong(context.Context, []byte) {
	h.heard()
}

// errFrameReaderStopped is what a read after the connection ended returns,
// once the error that ended it has been delivered.
var errFrameReaderStopped = errors.New("websocket reader stopped")

type frameResult struct {
	messageType websocket.MessageType
	frame       []byte
	err         error
}

func (r frameResult) cost() int {
	return len(r.frame) + queuedMessageOverhead
}

// newFrameReader starts reading with read, which runs under ctx. The goroutine
// ends after the first failed read, when ctx is done, or when stop is called.
// onEnded, if not nil, runs on the goroutine when the connection ends.
func newFrameReader(
	ctx context.Context,
	read func(context.Context) (websocket.MessageType, []byte, error),
	onEnded func(),
) *frameReader {
	stopped := make(chan struct{})
	ended := make(chan struct{})
	reader := &frameReader{
		read:      read,
		onEnded:   onEnded,
		stopped:   stopped,
		stop:      sync.OnceFunc(func() { close(stopped) }),
		done:      make(chan struct{}),
		ended:     ended,
		closeEnds: sync.OnceFunc(func() { close(ended) }),
		taken:     make(chan struct{}, 1),
		changed:   make(chan struct{}),
	}
	reader.lastHeard.Store(time.Now().UnixNano())
	go reader.run(ctx)
	return reader
}

// next returns the next message, or the error that ended the connection. A
// canceled ctx returns ctx.Err without waiting for the read in flight.
func (r *frameReader) next(ctx context.Context) (websocket.MessageType, []byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		result, ok, changed := r.pop()
		if ok {
			return result.messageType, result.frame, result.err
		}
		if changed == nil {
			return 0, nil, errFrameReaderStopped
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
}

// await blocks until a result is queued or the reader is finished, and reports
// false if ctx ended first. It takes nothing.
func (r *frameReader) await(ctx context.Context) bool {
	for {
		r.mu.Lock()
		ready := len(r.queue) > 0 || r.finished
		changed := r.changed
		r.mu.Unlock()
		if ready {
			return true
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

// drained reports whether the reader is finished and everything it read has
// been taken.
func (r *frameReader) drained() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finished && len(r.queue) == 0
}

// nextAfterEnd is next for a reader whose connection has ended: it returns
// what was read before the end, then the error that ended it. It needs no
// context because it never waits.
func (r *frameReader) nextAfterEnd() (websocket.MessageType, []byte, error) {
	if result, ok, _ := r.pop(); ok {
		return result.messageType, result.frame, result.err
	}
	return 0, nil, errFrameReaderStopped
}

// pop takes the oldest unread result. Without one, it returns the channel
// that will signal a change, or nil when the reader is finished.
func (r *frameReader) pop() (frameResult, bool, chan struct{}) {
	r.mu.Lock()
	if len(r.queue) == 0 {
		defer r.mu.Unlock()
		if r.finished {
			return frameResult{}, false, nil
		}
		return frameResult{}, false, r.changed
	}
	result := r.queue[0]
	r.queue[0] = frameResult{}
	r.queue = r.queue[1:]
	r.queuedBytes -= result.cost()
	r.mu.Unlock()
	select {
	case r.taken <- struct{}{}:
	default:
	}
	return result, true, nil
}

// close stops the goroutine and waits for it. The connection must already be
// closed, or the read in flight will not return.
func (r *frameReader) close() {
	r.stop()
	<-r.done
}

// hasEnded reports whether a read has already failed because the connection
// ended.
func (r *frameReader) hasEnded() bool {
	if r == nil {
		return false
	}
	select {
	case <-r.ended:
		return true
	default:
		return false
	}
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
// hearing from the peer. It is zero while read-ahead is full.
func (r *frameReader) silence(now time.Time) time.Duration {
	waitingSince := r.waitingSince.Load()
	if waitingSince == 0 {
		return 0
	}
	return now.Sub(time.Unix(0, max(waitingSince, r.lastHeard.Load())))
}

func (r *frameReader) run(ctx context.Context) {
	defer close(r.done)
	defer r.finish(nil)
	for {
		if !r.awaitRoom(ctx) {
			return
		}
		r.waitingSince.Store(time.Now().UnixNano())
		messageType, frame, err := r.read(ctx)
		r.waitingSince.Store(0)
		if err == nil {
			r.heard()
			r.push(frameResult{messageType: messageType, frame: frame})
			continue
		}
		// Queued before anyone is told, so a consumer woken by the end finds
		// the error that caused it.
		r.finish(&frameResult{err: err})
		if !errors.Is(err, errMessageTooBig) {
			r.closeEnds()
			if r.onEnded != nil {
				r.onEnded()
			}
		}
		return
	}
}

// awaitRoom waits until read-ahead has room, and reports false if the reader
// was stopped or its context ended first.
func (r *frameReader) awaitRoom(ctx context.Context) bool {
	for {
		r.mu.Lock()
		full := len(r.queue) > 0 && r.queuedBytes >= readAheadBytes
		r.mu.Unlock()
		if !full {
			return true
		}
		select {
		case <-r.taken:
		case <-r.stopped:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func (r *frameReader) push(result frameResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queue = append(r.queue, result)
	r.queuedBytes += result.cost()
	r.notifyLocked()
}

// finish marks the reader finished, queuing last first if there is one. Only
// the first call has any effect.
func (r *frameReader) finish(last *frameResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	if last != nil {
		r.queue = append(r.queue, *last)
		r.queuedBytes += last.cost()
	}
	r.finished = true
	r.notifyLocked()
}

func (r *frameReader) notifyLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}
