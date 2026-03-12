// Copyright 2021-2025 The Connect Authors
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

package memhttp

import (
	"io"
	"net"
	"sync"
	"time"
)

// bufferedPipeConn wraps a net.Conn with asynchronous write buffering.
// Writes are queued in an internal buffer and flushed to the underlying
// connection by a background goroutine. Reads and other operations go
// directly to the underlying connection.
type bufferedPipeConn struct {
	net.Conn

	mu       sync.Mutex
	cond     sync.Cond
	buf      []byte
	closed   bool
	writeErr error         // error from the writeLoop
	done     chan struct{} // closed when background goroutine exits
}

func newBufferedPipeConn(conn net.Conn) *bufferedPipeConn {
	buffered := &bufferedPipeConn{
		Conn: conn,
		done: make(chan struct{}),
	}
	buffered.cond.L = &buffered.mu
	go buffered.writeLoop()
	return buffered
}

func (bpc *bufferedPipeConn) Write(data []byte) (int, error) {
	bpc.mu.Lock()
	defer bpc.mu.Unlock()

	if bpc.writeErr != nil {
		return 0, bpc.writeErr
	}
	if bpc.closed {
		return 0, io.ErrClosedPipe
	}
	bpc.buf = append(bpc.buf, data...)
	bpc.cond.Signal()
	return len(data), nil
}

func (bpc *bufferedPipeConn) Close() error {
	bpc.mu.Lock()
	bpc.closed = true
	bpc.cond.Signal()
	bpc.mu.Unlock()
	<-bpc.done // wait for write loop to drain and exit
	return bpc.Conn.Close()
}

func (bpc *bufferedPipeConn) SetWriteDeadline(deadline time.Time) error {
	err := bpc.Conn.SetWriteDeadline(deadline)
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		bpc.mu.Lock()
		bpc.closed = true
		bpc.cond.Signal()
		bpc.mu.Unlock()
	}
	return err
}

func (bpc *bufferedPipeConn) SetDeadline(deadline time.Time) error {
	if err := bpc.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return bpc.Conn.SetReadDeadline(deadline)
}

func (bpc *bufferedPipeConn) writeLoop() {
	defer close(bpc.done)
	var flushBuf []byte
	for {
		bpc.mu.Lock()
		for len(bpc.buf) == 0 && !bpc.closed {
			bpc.cond.Wait()
		}

		// Fast swap: active buffer becomes the flush buffer,
		// and the flush buffer is reset to become the new active buffer.
		bpc.buf, flushBuf = flushBuf[:0], bpc.buf
		closed := bpc.closed
		bpc.mu.Unlock()

		if len(flushBuf) > 0 {
			if _, err := bpc.Conn.Write(flushBuf); err != nil {
				bpc.mu.Lock()
				if bpc.writeErr == nil {
					bpc.writeErr = err
				}
				// Force close to stop writers if the underlying conn fails
				bpc.closed = true
				bpc.mu.Unlock()
				return
			}
		}
		if closed {
			return
		}
	}
}
