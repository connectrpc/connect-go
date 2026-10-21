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

package connect

import (
	"bytes"
	"context"
	"io"
	"sync"

	connectv2 "connectrpc.com/connect/v2"
)

// bufferPool holds the staging buffers the codec adapters use to bridge v1's
// []byte codecs onto v2's io-based codecs.
var bufferPool = sync.Pool{ //nolint:gochecknoglobals
	New: func() any { return &bytes.Buffer{} },
}

// codecToConnectv2 adapts a v1 [Codec] to a [connectv2.Codec]. Codecs that
// implement stableCodec become a [connectv2.StableCodec].
func codecToConnectv2(codec Codec) connectv2.Codec {
	if codec == nil {
		return nil
	}
	adapter := v2CodecAdapter{v1: codec}
	if stable, ok := codec.(stableCodec); ok {
		return &v2StableCodecAdapter{v2CodecAdapter: adapter, stable: stable}
	}
	return &adapter
}

// availableBufferWriter is implemented by the writers v2 transports pass to
// MarshalWrite. Appending to AvailableBuffer encodes into the destination's
// spare capacity.
type availableBufferWriter interface {
	io.Writer
	AvailableBuffer() []byte
}

type v2CodecAdapter struct {
	v1 Codec
}

func (a *v2CodecAdapter) Name() string { return a.v1.Name() }

func (a *v2CodecAdapter) MarshalWrite(_ context.Context, dst io.Writer, msg any) error {
	appender, ok := a.v1.(marshalAppender)
	if !ok {
		raw, err := a.v1.Marshal(msg)
		if err != nil {
			return err
		}
		_, err = dst.Write(raw)
		return err
	}
	if buffered, ok := dst.(availableBufferWriter); ok {
		out, err := appender.MarshalAppend(buffered.AvailableBuffer(), msg)
		if err != nil {
			return err
		}
		_, err = buffered.Write(out)
		return err
	}
	buf, _ := bufferPool.Get().(*bytes.Buffer)
	defer putBuffer(buf)
	out, err := appender.MarshalAppend(buf.AvailableBuffer(), msg)
	if err != nil {
		return err
	}
	_, err = dst.Write(out)
	return err
}

func (a *v2CodecAdapter) UnmarshalRead(_ context.Context, src io.Reader, msg any) error {
	buf, _ := bufferPool.Get().(*bytes.Buffer)
	defer putBuffer(buf)
	if _, err := buf.ReadFrom(src); err != nil {
		return err
	}
	return a.v1.Unmarshal(buf.Bytes(), msg)
}

type v2StableCodecAdapter struct {
	v2CodecAdapter

	stable stableCodec
}

func (a *v2StableCodecAdapter) MarshalWriteStable(_ context.Context, dst io.Writer, msg any) error {
	raw, err := a.stable.MarshalStable(msg)
	if err != nil {
		return err
	}
	_, err = dst.Write(raw)
	return err
}

func (a *v2StableCodecAdapter) IsBinary() bool {
	return a.stable.IsBinary()
}

func putBuffer(buf *bytes.Buffer) {
	buf.Reset()
	bufferPool.Put(buf)
}
