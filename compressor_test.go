package connect

import (
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/bufbuild/connect/internal/assert"
)

func TestEmptyGzipBytes(t *testing.T) {
	_, err := gzip.NewReader(bytes.NewReader(emptyGzipBytes[:]))
	assert.Nil(t, err, "empty gzip bytes are invalid")
}
