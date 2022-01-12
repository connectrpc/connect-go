// Package callstream contains typed stream implementations from the caller's
// point of view.
package callstream

import (
	"context"

	"github.com/rerpc/rerpc"
)

// Client is the client's view of a client streaming RPC.
type Client[Req, Res any] struct {
	ctx    context.Context
	stream rerpc.Stream
}

// NewClient constructs a Client.
func NewClient[Req, Res any](ctx context.Context, stream rerpc.Stream) *Client[Req, Res] {
	return &Client[Req, Res]{ctx: ctx, stream: stream}
}

// Context returns the context for the stream. If the client is configured with
// interceptors, they've already had an opportunity to modify the context.
func (c *Client[Req, Res]) Context() context.Context {
	return c.ctx
}

// Header returns the headers. Headers are sent with the first call to Send.
func (c *Client[Req, Res]) Header() rerpc.Header {
	return c.stream.Header()
}

// Send a message to the server.
func (c *Client[Req, Res]) Send(msg *Req) error {
	return c.stream.Send(msg)
}

// CloseAndReceive closes the send side of the stream and waits for the
// response.
func (c *Client[Req, Res]) CloseAndReceive() (*Res, error) {
	if err := c.stream.CloseSend(nil); err != nil {
		return nil, err
	}
	var res Res
	if err := c.stream.Receive(&res); err != nil {
		_ = c.stream.CloseReceive()
		return nil, err
	}
	if err := c.stream.CloseReceive(); err != nil {
		return nil, err
	}
	return &res, nil
}

// ReceivedHeader returns the headers received from the server. It blocks until
// the response headers arrive.
func (c *Client[Req, Res]) ReceivedHeader() rerpc.Header {
	return c.stream.ReceivedHeader()
}

// Server is the client's view of a server streaming RPC.
type Server[Res any] struct {
	ctx    context.Context
	stream rerpc.Stream
}

// NewServer constructs a Server.
func NewServer[Res any](ctx context.Context, stream rerpc.Stream) *Server[Res] {
	return &Server[Res]{ctx: ctx, stream: stream}
}

// Context returns the context for the stream. If the client is configured with
// interceptors, they've already had an opportunity to modify the context.
func (s *Server[Res]) Context() context.Context {
	return s.ctx
}

// Receive a message. When the server is done sending messages, Receive will
// return an error that wraps io.EOF.
func (s *Server[Res]) Receive() (*Res, error) {
	var res Res
	if err := s.stream.Receive(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ReceivedHeader returns the headers received from the server. It blocks until
// the response headers arrive.
func (s *Server[Res]) ReceivedHeader() rerpc.Header {
	return s.stream.ReceivedHeader()
}

// Close the receive side of the stream.
func (s *Server[Res]) Close() error {
	return s.stream.CloseReceive()
}

// Bidirectional is the client's view of a bidirectional streaming RPC.
type Bidirectional[Req, Res any] struct {
	ctx    context.Context
	stream rerpc.Stream
}

// NewBidirectional constructs a Bidirectional.
func NewBidirectional[Req, Res any](
	ctx context.Context,
	stream rerpc.Stream,
) *Bidirectional[Req, Res] {
	return &Bidirectional[Req, Res]{ctx: ctx, stream: stream}
}

// Context returns the context for the stream. If the client is configured with
// interceptors, they've already had an opportunity to modify the context.
func (b *Bidirectional[Req, Res]) Context() context.Context {
	return b.ctx
}

// Header returns the headers. Headers are sent with the first call to Send.
func (b *Bidirectional[Req, Res]) Header() rerpc.Header {
	return b.stream.Header()
}

// Send a message to the server.
func (b *Bidirectional[Req, Res]) Send(msg *Req) error {
	return b.stream.Send(msg)
}

// CloseSend closes the send side of the stream.
func (b *Bidirectional[Req, Res]) CloseSend() error {
	return b.stream.CloseSend(nil)
}

// Receive a message. When the server is done sending messages, Receive will
// return an error that wraps io.EOF.
func (b *Bidirectional[Req, Res]) Receive() (*Res, error) {
	var res Res
	if err := b.stream.Receive(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

// CloseReceive closes the receive side of the stream.
func (b *Bidirectional[Req, Res]) CloseReceive() error {
	return b.stream.CloseReceive()
}

// ReceivedHeader returns the headers received from the server. It blocks until
// the response headers arrive.
func (b *Bidirectional[Req, Res]) ReceivedHeader() rerpc.Header {
	return b.stream.ReceivedHeader()
}
