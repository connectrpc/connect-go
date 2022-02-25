// Explain like I'm 2 again why we need this as a separate package.
package compress

import (
	"io"
)

// What is this for? I can just read the code but explaining this would be good. Should this be internal?
const NameIdentity = "identity"

// A Compressor provides compressing readers and writers. The interface is
// designed to let implementations use a sync.Pool.
//
// Additionally, Compressors contain logic to decide whether it's worth
// compressing a given payload. Often, it's not worth burning CPU cycles
// compressing small payloads.
type Compressor interface {
	GetReader(io.Reader) (io.ReadCloser, error)
	PutReader(io.ReadCloser)

	ShouldCompress([]byte) bool
	GetWriter(io.Writer) io.WriteCloser
	PutWriter(io.WriteCloser)
}
