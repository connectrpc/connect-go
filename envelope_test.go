// Copyright 2021-2023 The Connect Authors
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
	"encoding/binary"
	"io"
	"testing"

	"connectrpc.com/connect/internal/assert"
)

func TestEnvelope_read(t *testing.T) {
	t.Parallel()

	head := [5]byte{}
	payload := []byte(`{"number": 42}`)
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	buf := &bytes.Buffer{}
	buf.Write(head[:])
	buf.Write(payload)

	t.Run("full", func(t *testing.T) {
		t.Parallel()
		env := &envelope{Data: &bytes.Buffer{}}
		rdr := envelopeReader{}
		src := bytes.NewReader(buf.Bytes())
		assert.Nil(t, rdr.Read(env, src))
		assert.Equal(t, payload, env.Data.Bytes())
	})
	t.Run("byteByByte", func(t *testing.T) {
		t.Parallel()
		env := &envelope{Data: &bytes.Buffer{}}
		rdr := envelopeReader{}
		src := byteByByteReader{
			reader: bytes.NewReader(buf.Bytes()),
		}
		assert.Nil(t, rdr.Read(env, src))
		assert.Equal(t, payload, env.Data.Bytes())
	})
}

func TestEnvelope_write(t *testing.T) {
	t.Parallel()

	head := [5]byte{}
	payload := []byte(`{"number": 42}`)
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	buf := &bytes.Buffer{}
	buf.Write(head[:])
	buf.Write(payload)

	t.Run("match", func(t *testing.T) {
		t.Parallel()
		dst := &bytes.Buffer{}
		wtr := envelopeWriter{}
		env := &envelope{Data: bytes.NewBuffer(payload)}
		err := wtr.Write(dst, env)
		assert.Nil(t, err)
		assert.Equal(t, buf.Bytes(), dst.Bytes())
	})
	t.Run("partial", func(t *testing.T) {
		t.Parallel()
		dst := &bytes.Buffer{}
		env := &envelope{Data: bytes.NewBuffer(payload)}
		_, err := io.CopyN(dst, env, 2)
		assert.Nil(t, err)
		_, err = env.WriteTo(dst)
		assert.Nil(t, err)
		assert.Equal(t, buf.Bytes(), dst.Bytes())
	})
}

// byteByByteReader is test reader that reads a single byte at a time.
type byteByByteReader struct {
	reader io.ByteReader
}

func (b byteByByteReader) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	next, err := b.reader.ReadByte()
	if err != nil {
		return 0, err
	}
	data[0] = next
	return 1, nil
}
