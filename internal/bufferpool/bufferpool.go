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

// Package bufferpool provides a pool of reusable byte buffers shared by
// the codecs and transport.
package bufferpool

import (
	"bytes"
	"sync"
)

const (
	initialBufferSize    = 1024
	maxRecycleBufferSize = 8 * 1024 * 1024 // if >8MiB, don't recycle
)

//nolint:gochecknoglobals // package-level pool is the standard sync.Pool idiom
var pool = sync.Pool{
	New: func() any { return bytes.NewBuffer(make([]byte, 0, initialBufferSize)) },
}

// Get returns a reset buffer ready for use.
func Get() *bytes.Buffer {
	buf, ok := pool.Get().(*bytes.Buffer)
	if !ok {
		buf = bytes.NewBuffer(make([]byte, 0, initialBufferSize))
	}
	buf.Reset()
	return buf
}

// Put returns a buffer to the pool, dropping it if it grew past the
// recycle ceiling.
func Put(buf *bytes.Buffer) {
	if buf.Cap() > maxRecycleBufferSize {
		return
	}
	pool.Put(buf)
}
