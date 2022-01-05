// Package handlerstream contains typed stream implementations from the server's
// point of view.
package handlerstream

import "github.com/rerpc/rerpc"

// Client is the server's view of a client streaming RPC.
type Client[Req, Res any] struct {
	stream rerpc.Stream
}

// NewClient constructs a Client.
func NewClient[Req, Res any](stream rerpc.Stream) *Client[Req, Res] {
	return &Client[Req, Res]{stream: stream}
}

// ReceivedHeader returns the headers received from the client.
func (c *Client[Req, Res]) ReceivedHeader() rerpc.Header {
	return c.stream.ReceivedHeader()
}

// Receive a message. When the client is done sending messages, Receive returns
// an error that wraps io.EOF.
func (c *Client[Req, Res]) Receive() (*Req, error) {
	var req Req
	if err := c.stream.Receive(&req); err != nil {
		return nil, err
	}
	return &req, nil
}

// Header returns the response headers. Headers are sent when SendAndClose is
// called.
func (c *Client[Req, Res]) Header() rerpc.Header {
	return c.stream.Header()
}

// SendAndClose closes the receive side of the stream, then sends a response
// back to the client.
func (c *Client[Req, Res]) SendAndClose(msg *Res) error {
	if err := c.stream.CloseReceive(); err != nil {
		return err
	}
	return c.stream.Send(msg)
}

// Server is the server's view of a server streaming RPC.
type Server[Res any] struct {
	stream rerpc.Stream
}

// NewServer constructs a Server.
func NewServer[Res any](stream rerpc.Stream) *Server[Res] {
	return &Server[Res]{stream: stream}
}

// Header returns the response headers. Headers are sent with the first call to
// Send.
func (s *Server[Res]) Header() rerpc.Header {
	return s.stream.Header()
}

// Send a message to the client.
func (s *Server[Res]) Send(msg *Res) error {
	return s.stream.Send(msg)
}

// Bidirectional is the server's view of a bidirectional streaming RPC.
type Bidirectional[Req, Res any] struct {
	stream rerpc.Stream
}

// NewBidirectional constructs a Bidirectional.
func NewBidirectional[Req, Res any](stream rerpc.Stream) *Bidirectional[Req, Res] {
	return &Bidirectional[Req, Res]{stream: stream}
}

// ReceivedHeader returns the headers received from the client.
func (b *Bidirectional[Req, Res]) ReceivedHeader() rerpc.Header {
	return b.stream.ReceivedHeader()
}

// Receive a message. When the client is done sending messages, Receive will
// return an error that wraps io.EOF.
func (b *Bidirectional[Req, Res]) Receive() (*Req, error) {
	var req Req
	if err := b.stream.Receive(&req); err != nil {
		return nil, err
	}
	return &req, nil
}

// Header returns the response headers. Headers are sent with the first call to
// Send.
func (b *Bidirectional[Req, Res]) Header() rerpc.Header {
	return b.stream.Header()
}

func (b *Bidirectional[Req, Res]) Send(msg *Res) error {
	return b.stream.Send(msg)
}
