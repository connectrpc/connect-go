package connect

import (
	"strings"

	"github.com/bufconnect/connect/compress"
)

// readOnlyCompressors is a read-only interface to a map of named compressors.
type readOnlyCompressors interface {
	Get(string) compress.Compressor
	Contains(string) bool
	Names() string
}

type compressorMap struct {
	compressors map[string]compress.Compressor
	names       string
}

func newReadOnlyCompressors(compressors map[string]compress.Compressor) *compressorMap {
	known := make([]string, 0, len(compressors))
	for name := range compressors {
		known = append(known, name)
	}
	return &compressorMap{
		compressors: compressors,
		names:       strings.Join(known, ","),
	}
}

func (m *compressorMap) Get(name string) compress.Compressor {
	if name == "" || name == compress.NameIdentity {
		return nil
	}
	return m.compressors[name]
}

func (m *compressorMap) Contains(name string) bool {
	_, ok := m.compressors[name]
	return ok
}

func (m *compressorMap) Names() string {
	return m.names
}
