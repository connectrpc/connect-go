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

import "context"

// A UnaryInterceptorFunc is a simple Interceptor implementation that only
// wraps unary RPCs. It has no effect on client, server, or bidirectional
// streaming RPCs.
type UnaryInterceptorFunc func(Func) Func

// WrapUnary implements Interceptor by applying the interceptor function.
func (f UnaryInterceptorFunc) WrapUnary(next Func) Func { return f(next) }

// WrapStreamContext implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapStreamContext(ctx context.Context) context.Context {
	return ctx
}

// WrapStreamSender implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapStreamSender(_ context.Context, sender Sender) Sender {
	return sender
}

// WrapStreamReceiver implements Interceptor with a no-op.
func (f UnaryInterceptorFunc) WrapStreamReceiver(_ context.Context, receiver Receiver) Receiver {
	return receiver
}
