// Package callstream contains typed stream implementations from the caller's
// point of view.
package callstream

import "github.com/rerpc/rerpc"

// Client is the client's view of a client streaming RPC.
type Client[Req, Res any] struct {
	stream rerpc.Stream
}

// NewClient constructs a Client.
func NewClient[Req, Res any](stream rerpc.Stream) *Client[Req, Res] {
	return &Client[Req, Res]{stream: stream}
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

// Server is the client's view of a server streaming RPC.
type Server[Res any] struct {
	stream rerpc.Stream
}

// NewServer constructs a Server.
func NewServer[Res any](stream rerpc.Stream) *Server[Res] {
	return &Server[Res]{stream: stream}
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

// Close the receive side of the stream.
func (s *Server[Res]) Close() error {
	return s.stream.CloseReceive()
}

// Bidirectional is the client's view of a bidirectional streaming RPC.
type Bidirectional[Req, Res any] struct {
	stream rerpc.Stream
}

// NewBidirectional constructs a Bidirectional.
func NewBidirectional[Req, Res any](stream rerpc.Stream) *Bidirectional[Req, Res] {
	return &Bidirectional[Req, Res]{stream: stream}
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

