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
	"compress/gzip"
	"io"
	"strings"
	"testing"

	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
)

func TestBuffers_ReadAllAllocs(t *testing.T) {
	t.Parallel()
	str := "hello world" + strings.Repeat("b", bytes.MinRead)
	src := strings.NewReader(str)
	buf := &bytes.Buffer{}

	avg := testing.AllocsPerRun(4, func() {
		_, _ = src.Seek(0, 0)
		buf.Reset()
		if err := readAll(buf, src, maxRecycleBufferSize); err != nil {
			t.Fatal(err)
		}
	})
	t.Log(avg)
	assert.Equal(t, str, buf.String())
	assert.True(t, avg <= 1.0)
}

func TestBuffers_ReadEnvelopeAllocs(t *testing.T) {
	t.Parallel()
	env := bytes.Buffer{}
	str := "hello world" + strings.Repeat("b", bytes.MinRead)
	assert.Nil(t, writeEnvelope(&env, bytes.NewBufferString(str), 0))
	env.Write(make([]byte, 0, maxRecycleBufferSize)) // large stream, greater than maxRecycledBufferSize
	src := bytes.NewReader(env.Bytes())
	buf := &bytes.Buffer{}

	avg := testing.AllocsPerRun(4, func() {
		_, _ = src.Seek(0, 0)
		buf.Reset()
		_, err := readEnvelope(buf, src, maxRecycleBufferSize)
		assert.Nil(t, err)
	})
	t.Log(avg)
	assert.Equal(t, len(str), buf.Len())
	assert.Equal(t, str, buf.String())
	assert.True(t, avg <= 2.0)
}

func TestBuffers_Marshal(t *testing.T) {
	t.Parallel()
	codec := &protoBinaryCodec{}
	msg := &pingv1.PingRequest{Text: "hello world"}
	buf := &bytes.Buffer{}

	avg := testing.AllocsPerRun(4, func() {
		buf.Reset()
		assert.Nil(t, marshal(buf, msg, codec))
		assert.Nil(t, unmarshal(buf, msg, codec))
		assert.Equal(t, "hello world", msg.Text)
	})
	t.Log(avg)
	assert.True(t, avg <= 16.0)
	assert.True(t, strings.Contains(buf.String(), "hello world"))
}

func TestBuffers_Compress(t *testing.T) {
	t.Parallel()
	pool := &bufferPool{}
	input := `{"text":"` + strings.Repeat("a", bytes.MinRead) + `"}`
	src := bytes.NewBufferString(input)
	comp := newCompressionPool(
		func() Decompressor { return &gzip.Reader{} },
		func() Compressor { return gzip.NewWriter(io.Discard) },
	)

	avg := testing.AllocsPerRun(4, func() {
		assert.Nil(t, comp.Compress(pool, src))
		assert.True(t, src.Len() < len(input))
		assert.Nil(t, comp.Decompress(pool, src, 0))
		assert.True(t, src.Len() == len(input))
	})
	t.Log(avg)
	// Don't check avg because it's dependent on buffer.Pool's behavior.
	assert.Equal(t, input, src.String())
}
