package rerpc

import (
	"strings"

	"github.com/rerpc/rerpc/compress"
)

// roCompressors is a read-only interface to a map of named compressors.
type roCompressors interface {
	Get(string) compress.Compressor
	Contains(string) bool
	Names() string
}

type compressorMap struct {
	m     map[string]compress.Compressor
	names string
}

func newROCompressors(m map[string]compress.Compressor) *compressorMap {
	known := make([]string, 0, len(m))
	for name := range m {
		known = append(known, name)
	}
	return &compressorMap{
		m:     m,
		names: strings.Join(known, ","),
	}
}

func (m *compressorMap) Get(name string) compress.Compressor {
	if name == "" || name == compress.NameIdentity {
		return nil
	}
	return m.m[name]
}

func (m *compressorMap) Contains(name string) bool {
	_, ok := m.m[name]
	return ok
}

func (m *compressorMap) Names() string {
	return m.names
}
