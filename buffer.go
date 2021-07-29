package rerpc

import (
	"bytes"
	"sync"
)

var bufferPool = sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, 512))
	},
}

func getBuffer() *bytes.Buffer {
	return bufferPool.Get().(*bytes.Buffer)
}

func putBuffer(buf *bytes.Buffer) {
	const max = 4 * 1024 // if >4 KiB, don't hold onto it
	if buf.Cap() > max {
		return
	}
	buf.Reset()
	bufferPool.Put(buf)
}
