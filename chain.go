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
	"context"
)

// A Chain composes multiple interceptors into one.
type chain struct {
	interceptors []Interceptor
}

var _ Interceptor = (*chain)(nil)

// NewChain composes multiple interceptors into one.
func newChain(interceptors []Interceptor) *chain {
	// We usually wrap in reverse order to have the first interceptor from
	// the slice act first. Rather than doing this dance repeatedly, reverse the
	// interceptor order now.
	var c chain
	for i := len(interceptors) - 1; i >= 0; i-- {
		if interceptor := interceptors[i]; interceptor != nil {
			c.interceptors = append(c.interceptors, interceptor)
		}
	}
	return &c
}

func (c *chain) WrapUnary(next Func) Func {
	for _, interceptor := range c.interceptors {
		next = interceptor.WrapUnary(next)
	}
	return next
}

func (c *chain) WrapStreamContext(ctx context.Context) context.Context {
	for _, interceptor := range c.interceptors {
		ctx = interceptor.WrapStreamContext(ctx)
	}
	return ctx
}

func (c *chain) WrapStreamSender(ctx context.Context, sender Sender) Sender {
	if sender.Spec().IsClient {
		for _, interceptor := range c.interceptors {
			sender = interceptor.WrapStreamSender(ctx, sender)
		}
		return sender
	}
	// When we're wrapping senders on the handler side, we need to wrap in the
	// opposite order. See TestOnionOrderingEndToEnd.
	for i := len(c.interceptors) - 1; i >= 0; i-- {
		if interceptor := c.interceptors[i]; interceptor != nil {
			sender = interceptor.WrapStreamSender(ctx, sender)
		}
	}
	return sender
}

func (c *chain) WrapStreamReceiver(ctx context.Context, receiver Receiver) Receiver {
	for _, interceptor := range c.interceptors {
		receiver = interceptor.WrapStreamReceiver(ctx, receiver)
	}
	return receiver
}
