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

// Compression pooling for the Connect, gRPC, and gRPC-Web protocols.
//
// This lived in internal/compression while connectwebsocket shared it. That
// transport compresses whole messages with permessage-deflate instead, below
// the protocol, so the pooling came home rather than staying factored out for
// a single caller.

package connecthttp

import (
	"bytes"
	"io"
	"math"
	"strings"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/connecterr"
)

// compressionPool applies one [connect.Compressor] to whole messages.
type compressionPool struct {
	compressor connect.Compressor
}

// newCompressionPool returns a [compressionPool] for compressor, or nil when compressor is nil.
func newCompressionPool(compressor connect.Compressor) *compressionPool {
	if compressor == nil {
		return nil
	}
	return &compressionPool{
		compressor: compressor,
	}
}

// Decompress expands src into dst, failing with
// [connect.CodeResourceExhausted] when the result would exceed readMaxBytes. A
// readMaxBytes of zero means no limit.
func (c *compressionPool) Decompress(dst *bytes.Buffer, src *bytes.Buffer, readMaxBytes int64) *connect.Error {
	decompressor, err := c.compressor.Decompress(src)
	if err != nil {
		return connect.Errorf(connect.CodeInvalidArgument, "get decompressor: %s", err).WithCause(err)
	}
	defer decompressor.Close()
	reader := io.Reader(decompressor)
	if readMaxBytes > 0 && readMaxBytes < math.MaxInt64 {
		reader = io.LimitReader(decompressor, readMaxBytes+1)
	}
	bytesRead, err := dst.ReadFrom(reader)
	if err != nil {
		err = connecterr.WrapIfContextError(err)
		if connectErr, ok := connecterr.AsError(err); ok {
			return connectErr
		}
		return connect.Errorf(connect.CodeInvalidArgument, "decompress: %s", err).WithCause(err)
	}
	if readMaxBytes > 0 && bytesRead > readMaxBytes {
		discardedBytes, err := io.Copy(io.Discard, decompressor)
		if err != nil {
			return connect.Errorf(connect.CodeResourceExhausted, "message is larger than configured max %d - unable to determine message size: %s", readMaxBytes, err).WithCause(err)
		}
		return connect.Errorf(connect.CodeResourceExhausted, "message size %d is larger than configured max %d", bytesRead+discardedBytes, readMaxBytes)
	}
	return nil
}

// Compress writes the compressed form of src into dst.
func (c *compressionPool) Compress(dst *bytes.Buffer, src *bytes.Buffer) *connect.Error {
	compressor, err := c.compressor.Compress(dst)
	if err != nil {
		return connect.Errorf(connect.CodeUnknown, "get compressor: %s", err).WithCause(err)
	}
	defer compressor.Close()
	if _, err := src.WriteTo(compressor); err != nil {
		err = connecterr.WrapIfContextError(err)
		if connectErr, ok := connecterr.AsError(err); ok {
			return connectErr
		}
		return connect.Errorf(connect.CodeInternal, "compress: %s", err).WithCause(err)
	}
	return nil
}

// readOnlyCompressionPools is a read-only interface to a map of named [compressionPool]s.
type readOnlyCompressionPools interface {
	Get(string) *compressionPool
	Contains(string) bool
	// Wordy, but clarifies how this is different from a codec registry's
	// Names().
	CommaSeparatedNames() string
}

// newReadOnlyCompressionPools indexes compressors by name, in preference order. A
// repeated name is ignored.
func newReadOnlyCompressionPools(compressors []connect.Compressor) readOnlyCompressionPools {
	nameToPool := make(map[string]*compressionPool, len(compressors))
	names := make([]string, 0, len(compressors))
	for _, compressor := range compressors {
		name := compressor.Name()
		if _, ok := nameToPool[name]; ok {
			continue
		}
		nameToPool[name] = newCompressionPool(compressor)
		names = append(names, name)
	}
	return &namedCompressionPools{
		nameToPool:          nameToPool,
		commaSeparatedNames: strings.Join(names, ","),
	}
}

type namedCompressionPools struct {
	nameToPool          map[string]*compressionPool
	commaSeparatedNames string
}

func (m *namedCompressionPools) Get(name string) *compressionPool {
	if name == "" || name == connect.CompressionNameIdentity {
		return nil
	}
	return m.nameToPool[name]
}

func (m *namedCompressionPools) Contains(name string) bool {
	_, ok := m.nameToPool[name]
	return ok
}

func (m *namedCompressionPools) CommaSeparatedNames() string {
	return m.commaSeparatedNames
}
