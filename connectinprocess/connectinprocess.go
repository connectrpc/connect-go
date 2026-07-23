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

// Package connectinprocess provides an in-process [connect.Transport]. The
// transport dispatches every RPC directly to a [connect.Server] inside the
// same process, completely bypassing the network. It is ideal for testing and
// specific production workloads.
package connectinprocess

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
)

// CopyFunc copies a message from src into dst. The transport uses it to
// transfer each request and response between the client and server halves
// of an in-process RPC.
//
// The default is [ProtoCopy], a safe deep copy for Protobuf message. Override
// it with [WithCopyFunc].
//
// A CopyFunc must be safe to call from independent goroutines.
type CopyFunc func(dst, src any) error

// Option configures a Transport built by [New].
type Option interface{ apply(*options) }

// New returns a [connect.Transport] that dispatches every RPC in-process
// to the given server. Pass the returned transport into
// [connect.NewClient] when constructing your generated service clients.
func New(server *connect.Server, opts ...Option) connect.Transport {
	resolved := options{copy: ProtoCopy}
	for _, opt := range opts {
		opt.apply(&resolved)
	}
	return &transport{
		server: server,
		copy:   resolved.copy,
	}
}

// WithCopyFunc replaces the default [ProtoCopy] strategy.
func WithCopyFunc(fn CopyFunc) Option {
	return optionFunc(func(o *options) { o.copy = fn })
}

// ProtoCopy is the default [CopyFunc]. It uses [proto.Reset] followed by
// [proto.Merge] to perform a safe deep copy, so the client and server never
// share message state. It also bridges dynamic and generated messages of the
// same logical type.
func ProtoCopy(dst, src any) error {
	dstMsg, ok := dst.(proto.Message)
	if !ok {
		return fmt.Errorf("connectinprocess: ProtoCopy dst is %T, not a proto.Message", dst)
	}
	srcMsg, ok := src.(proto.Message)
	if !ok {
		return fmt.Errorf("connectinprocess: ProtoCopy src is %T, not a proto.Message", src)
	}
	proto.Reset(dstMsg)
	proto.Merge(dstMsg, srcMsg)
	return nil
}

type transport struct {
	server *connect.Server
	copy   CopyFunc
}

func (t *transport) NewClientStream(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
	if t.server == nil {
		return nil, errors.New("connectinprocess: nil server")
	}
	if spec.StreamType == connect.StreamTypeUnary {
		return newUnaryClientStream(ctx, t, spec), nil
	}
	return &clientStream{p: newStreamPair(ctx, t, spec)}, nil
}

type options struct {
	copy CopyFunc
}

type optionFunc func(*options)

func (f optionFunc) apply(o *options) { f(o) }
