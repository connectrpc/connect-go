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

package connectwebsocket

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"
)

// Selector reports whether spec's RPC should travel over WebSocket. A false
// result routes the RPC to the fallback transport instead.
type Selector func(spec connect.Spec) bool

// SelectStreaming routes every streaming RPC over WebSocket and leaves unary
// RPCs on the fallback transport. It is the default [Selector].
func SelectStreaming(spec connect.Spec) bool {
	return spec.StreamType != connect.StreamTypeUnary
}

// SelectBidi routes only bidirectional RPCs over WebSocket. Server-streaming
// works over plain HTTP, so this is the narrower alternative to
// [SelectStreaming].
func SelectBidi(spec connect.Spec) bool {
	return spec.StreamType == connect.StreamTypeBidi
}

// SelectAll routes every RPC over WebSocket, leaving the HTTP fallback unused.
func SelectAll(connect.Spec) bool {
	return true
}

// newHybridTransport returns a [connect.Transport] that opens each stream on
// websocket or fallback according to the configured [Selector].
//
// Either transport may be nil: an RPC routed to a nil transport fails with
// [connect.CodeUnimplemented].
func newHybridTransport(websocket, fallback connect.Transport, opts *options) connect.Transport {
	return &hybridTransport{
		websocket:       websocket,
		fallback:        fallback,
		selector:        opts.selector,
		fallbackOnError: opts.fallbackOnUpgradeError,
	}
}

type hybridTransport struct {
	websocket       connect.Transport
	fallback        connect.Transport
	selector        Selector
	fallbackOnError bool
}

func (t *hybridTransport) NewClientStream(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
	if t.websocket == nil || !t.selector(spec) {
		return t.fallbackStream(ctx, spec)
	}
	stream, err := t.websocket.NewClientStream(ctx, spec)
	if err == nil {
		return stream, nil
	}
	// A canceled call is the caller's doing, not a peer that lacks WebSocket
	// support, so retrying on the fallback would only obscure the cause.
	if !t.fallbackOnError || t.fallback == nil || ctx.Err() != nil {
		return nil, err
	}
	stream, fallbackErr := t.fallbackStream(ctx, spec)
	if fallbackErr != nil {
		return nil, errors.Join(err, fallbackErr)
	}
	// The WebSocket error is dropped here. A fallback transport is typically
	// lazy, so it usually succeeds at this point and fails later from Send, by
	// which time only its own error is left to report.
	return stream, nil
}

func (t *hybridTransport) fallbackStream(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
	if t.fallback == nil {
		return nil, connect.Errorf(
			connect.CodeUnimplemented,
			"connectwebsocket: no transport for %s (%s)",
			spec.Procedure, spec.StreamType,
		)
	}
	return t.fallback.NewClientStream(ctx, spec)
}
