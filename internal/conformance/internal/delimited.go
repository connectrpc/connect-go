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

package internal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
)

// ReadDelimitedMessage reads the next message from in. This first reads a
// fixed four byte preface, which is a network-encoded (i.e. big-endian)
// 32-bit integer that represents the message size. This then reads a
// number of bytes equal to that size and unmarshals it into msg.
func ReadDelimitedMessage[T proto.Message](in io.Reader, msg T, source string, timeout time.Duration, maxSize int) error {
	reader := timeoutDelimitedReader{
		in:       in,
		source:   source,
		timeout:  timeout,
		maxSize:  maxSize,
		readDone: make(chan struct{}),
	}
	data, err := reader.readDelimitedMessageRaw()
	if err != nil {
		return err
	}
	if err := proto.Unmarshal(data, msg); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}
	return nil
}

// WriteDelimitedMessage writes msg to out in a way that can be read by ReadDelimitedMessage.
func WriteDelimitedMessage[T proto.Message](out io.Writer, msg T) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}
	return writeDelimitedMessageRaw(out, data)
}

func writeDelimitedMessageRaw(out io.Writer, data []byte) error {
	var lenBuffer [4]byte
	binary.BigEndian.PutUint32(lenBuffer[:], uint32(len(data)))
	if _, err := out.Write(lenBuffer[:]); err != nil {
		return err
	}
	if _, err := out.Write(data); err != nil {
		return err
	}
	return nil
}

type timeoutDelimitedReader struct {
	in                        io.Reader
	source                    string
	timeout                   time.Duration
	maxSize                   int
	readDone                  chan struct{}
	mu                        sync.Mutex
	prefixDone                bool
	bytesRead, bytesExpecting int
}

func (r *timeoutDelimitedReader) readDelimitedMessageRaw() ([]byte, error) {
	var msgBytes []byte
	var readErr error
	readDone := make(chan struct{})

	r.bytesExpecting = 4 // prefix length
	go func() {
		defer close(readDone)
		var data []byte
		data, readErr = r.read(4)
		if readErr != nil {
			return
		}
		msgSize := int(binary.BigEndian.Uint32(data))
		if msgSize > r.maxSize {
			readErr = fmt.Errorf("%s result indicates message size of %d bytes, but should not exceed %d",
				r.source, msgSize, r.maxSize)
			return
		}
		r.mu.Lock()
		r.prefixDone, r.bytesRead, r.bytesExpecting = true, 0, msgSize
		r.mu.Unlock()
		msgBytes, readErr = r.read(msgSize)
		if errors.Is(readErr, io.EOF) {
			readErr = io.ErrUnexpectedEOF
		}
	}()

	select {
	case <-readDone:
		return msgBytes, readErr
	case <-time.After(r.timeout):
	}
	r.mu.Lock()
	prefixDone, bytesRead, bytesExpecting := r.prefixDone, r.bytesRead, r.bytesExpecting
	r.mu.Unlock()
	if prefixDone && bytesRead == bytesExpecting {
		// Read is actually complete and we are just racing with goroutine closing readDone.
		<-readDone
		return msgBytes, readErr
	}
	if !prefixDone && bytesRead == 0 {
		// we've read nothing at all, so no details to include
		return nil, fmt.Errorf("timed out waiting for result from %s", r.source)
	}
	var what string
	if prefixDone {
		what = "message"
	} else {
		what = "length prefix"
	}
	return nil, fmt.Errorf("timed out waiting for result from %s: read %d/%d bytes of %s",
		r.source, bytesRead, bytesExpecting, what)
}

func (r *timeoutDelimitedReader) read(numBytes int) ([]byte, error) {
	data := make([]byte, numBytes)
	var offs int
	for {
		numRead, err := r.in.Read(data[offs:])
		if offs+numRead == numBytes {
			// Done! If n > 0 and err != nil, we can
			// ignore the error and subsequent attempt
			// to read from in will return it.
			return data, nil
		}
		offs += numRead
		r.mu.Lock()
		r.bytesRead = offs // update progress as we go
		r.mu.Unlock()
		if err != nil {
			if errors.Is(err, io.EOF) && offs > 0 {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
}
