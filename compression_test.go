// Copyright 2021-2022 Buf Technologies, Inc.
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
	"io"
	"testing"

	"github.com/bufbuild/connect-go/internal/assert"
)

func TestReadOnlyCompressionPools(t *testing.T) {
	t.Parallel()
	names := []string{"foo", "foo", "bar", "baz"}
	compressionPools := map[string]*compressionPool{}
	compressionNames := []string{}
	for _, name := range names {
		name := name
		compressionPools[name] = newCompressionPool(
			func() Decompressor { return noopDecompressor{name} },
			func() Compressor { return noopCompressor{name} },
		)
		compressionNames = append(compressionNames, name)
	}
	pools := newReadOnlyCompressionPools(
		compressionPools,
		compressionNames,
	)
	t.Run("Get", func(t *testing.T) {
		for _, name := range names {
			pool := pools.Get(name)
			compressor := pool.compressors.Get().(noopCompressor)
			assert.Equal(t, compressor.name, name)
			pool.compressors.Put(compressor)
			decompressor := pool.decompressors.Get().(noopDecompressor)
			assert.Equal(t, decompressor.name, name)
			pool.decompressors.Put(decompressor)
		}
	})
	t.Run("Contains", func(t *testing.T) {
		for _, name := range names {
			assert.True(t, pools.Contains(name))
		}
		assert.False(t, pools.Contains("nope"))
	})
	t.Run("CommaSeparatedNames", func(t *testing.T) {
		assert.Equal(t, pools.CommaSeparatedNames(), "baz,bar,foo") // reversed and deduped
	})
}

type noopCompressor struct{ name string }

func (noopCompressor) Write(p []byte) (int, error) {
	return len(p), nil
}
func (noopCompressor) Close() error    { return nil }
func (noopCompressor) Reset(io.Writer) {}

type noopDecompressor struct{ name string }

func (noopDecompressor) Read(p []byte) (int, error) {
	return len(p), nil
}
func (noopDecompressor) Close() error          { return nil }
func (noopDecompressor) Reset(io.Reader) error { return nil }
