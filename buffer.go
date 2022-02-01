package connect

import (
	"bytes"
	"sync"
)

var bufferPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 512))
	},
}

func getBuffer() *bytes.Buffer {
	return bufferPool.Get().(*bytes.Buffer)
}

func putBuffer(buf *bytes.Buffer) {
	const max = 1024 * 1024 // if >1 MiB, don't hold onto it
	if buf.Cap() > max {
		return
	}
	buf.Reset()
	bufferPool.Put(buf)
}
