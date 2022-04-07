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
	"errors"
	"io"
	"net/http"
)

// ClientStream is the handler's view of a client streaming RPC.
type ClientStream[Req, Res any] struct {
	sender   Sender
	receiver Receiver
	msg      Req
	err      error
}

// NewClientStream constructs the handler's view of a client streaming RPC.
func NewClientStream[Req, Res any](s Sender, r Receiver) *ClientStream[Req, Res] {
	return &ClientStream[Req, Res]{sender: s, receiver: r}
}

// RequestHeader returns the headers received from the client.
func (c *ClientStream[Req, Res]) RequestHeader() http.Header {
	return c.receiver.Header()
}

// Receive advances the stream to the next message, which will then be
// available through the Msg method. It returns false when the stream stops,
// either by reaching the end or by encountering an unexpected error. After
// Receive returns false, the Err method will return any unexpected error
// encountered.
func (c *ClientStream[Req, Res]) Receive() bool {
	if c.err != nil {
		return false
	}
	c.err = c.receiver.Receive(&c.msg)
	return c.err == nil
}

// Msg returns the most recent message unmarshaled by a call to Receive. The
// returned message points to data that will be overwritten by the next call to
// Receive.
func (c *ClientStream[Req, Res]) Msg() *Req {
	return &c.msg
}

// Err returns the first non-EOF error that was encountered by Receive.
func (c *ClientStream[Req, Res]) Err() error {
	if c.err == nil || errors.Is(c.err, io.EOF) {
		return nil
	}
	return c.err
}

// SendAndClose closes the receive side of the stream, then sends a response
// back to the client.
func (c *ClientStream[Req, Res]) SendAndClose(envelope *Response[Res]) error {
	if err := c.receiver.Close(); err != nil {
		return err
	}
	mergeHeaders(c.sender.Header(), envelope.header)
	mergeHeaders(c.sender.Trailer(), envelope.trailer)
	return c.sender.Send(envelope.Msg)
}

// ServerStream is the handler's view of a server streaming RPC.
type ServerStream[Res any] struct {
	sender Sender
}

// NewServerStream constructs the handler's view of a server streaming RPC.
func NewServerStream[Res any](s Sender) *ServerStream[Res] {
	return &ServerStream[Res]{sender: s}
}

// ResponseHeader returns the response headers. Headers are sent with the first
// call to Send.
func (s *ServerStream[Res]) ResponseHeader() http.Header {
	return s.sender.Header()
}

// ResponseTrailer returns the response trailers. Handlers may write to the
// response trailers at any time before returning.
func (s *ServerStream[Res]) ResponseTrailer() http.Header {
	return s.sender.Trailer()
}

// Send a message to the client. The first call to Send also sends the response
// headers.
func (s *ServerStream[Res]) Send(msg *Res) error {
	return s.sender.Send(msg)
}

// BidiStream is the handler's view of a bidirectional streaming RPC.
type BidiStream[Req, Res any] struct {
	sender   Sender
	receiver Receiver
}

// NewBidiStream constructs the handler's view of a bidirectional streaming RPC.
func NewBidiStream[Req, Res any](s Sender, r Receiver) *BidiStream[Req, Res] {
	return &BidiStream[Req, Res]{sender: s, receiver: r}
}

// RequestHeader returns the headers received from the client.
func (b *BidiStream[Req, Res]) RequestHeader() http.Header {
	return b.receiver.Header()
}

// Receive a message. When the client is done sending messages, Receive will
// return an error that wraps io.EOF.
func (b *BidiStream[Req, Res]) Receive() (*Req, error) {
	var req Req
	if err := b.receiver.Receive(&req); err != nil {
		return nil, err
	}
	return &req, nil
}

// ResponseHeader returns the response headers. Headers are sent with the first
// call to Send.
func (b *BidiStream[Req, Res]) ResponseHeader() http.Header {
	return b.sender.Header()
}

// ResponseTrailer returns the response trailers. Handlers may write to the
// response trailers at any time before returning.
func (b *BidiStream[Req, Res]) ResponseTrailer() http.Header {
	return b.sender.Trailer()
}

// Send a message to the client. The first call to Send also sends the response
// headers.
func (b *BidiStream[Req, Res]) Send(msg *Res) error {
	return b.sender.Send(msg)
}
