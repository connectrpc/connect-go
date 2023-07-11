// Copyright 2021-2023 Buf Technologies, Inc.
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

package connect

import (
	"bytes"
	"io"
	"sync"
)

const maxRPCClientBufferSize = 256 * 1024 // 256KB

// messagePipe is based on io.Pipe.
// See: https://github.com/golang/go/blob/cb7a091d729eab75ccfdaeba5a0605f05addf422/src/io/pipe.go#L39
// And: https://github.com/golang/go/blob/cb7a091d729eab75ccfdaeba5a0605f05addf422/src/net/net_fake.go#L259
type messagePipe struct {
	mu   sync.Mutex
	wait sync.Cond
	werr error
	data []byte

	// buffering
	limit  int
	head   []byte              // TODO: *bytes.Buffer
	buffer *bytes.Buffer       // TODO: []*bytes.Buffer
	onFree func(*bytes.Buffer) // TODO: func([]*bytes.Buffer)
}

func (p *messagePipe) lock() {
	p.mu.Lock()
	if p.wait.L == nil {
		p.wait.L = &p.mu
	}
}
func (p *messagePipe) unlock() {
	p.mu.Unlock()
}

func (p *messagePipe) Read(data []byte) (int, error) {
	p.lock()
	defer p.unlock()
	for {
		switch {
		case p.data != nil:
			nbytes := copy(data, p.data)
			p.data = p.data[nbytes:]
			if len(p.data) == 0 {
				p.data = nil
				p.wait.Broadcast()
			}
			return nbytes, nil
		case p.werr != nil:
			return 0, p.werr
		}
		p.wait.Wait()
	}
}

// TODO: WriteMessage(*bytes.Buffer) (int, error)
func (p *messagePipe) Write(data []byte) (int, error) {
	if data == nil {
		var zero = [0]byte{}
		data = zero[:]
	}

	p.lock()
	defer p.unlock()
	for p.data != nil {
		if p.werr != nil {
			return 0, p.werr
		}
		p.wait.Wait()
	}
	if p.werr != nil {
		return 0, p.werr
	}
	p.data = data
	p.head = data
	p.wait.Broadcast()
	for {
		switch {
		case p.data == nil:
			if p.buffer != nil {
				if p.buffer.Len()+len(p.head) < p.limit {
					p.buffer.Write(p.head)
				} else {
					p.freeWithLock()
				}
			}
			p.head = nil
			return len(data), nil
		case p.werr != nil:
			nbytes := len(data) - len(p.data)
			p.data = nil
			p.head = nil
			err := p.werr
			return nbytes, err
		}
		p.wait.Wait()
	}
}

func (p *messagePipe) CloseWithErr(err error) {
	if err == nil {
		err = io.EOF
	}
	p.lock()
	defer p.unlock()
	p.werr = err
	p.wait.Broadcast()
}
func (p *messagePipe) Close() error {
	p.CloseWithErr(nil)
	return nil
}

func (p *messagePipe) Rewind() bool {
	p.lock()
	defer p.unlock()
	if p.buffer == nil {
		return false
	}
	if p.buffer.Len() > 0 {
		p.buffer.Write(p.head)
		p.head = nil
		p.data = p.buffer.Bytes()
	} else {
		// referenced the head
		p.data = p.head
	}
	p.wait.Broadcast()
	return true
}
func (p *messagePipe) Free() {
	p.lock()
	defer p.unlock()
	p.freeWithLock()
}
func (p *messagePipe) freeWithLock() {
	if p.onFree != nil {
		p.onFree(p.buffer)
		p.onFree = nil
	}
	p.buffer = nil
}
