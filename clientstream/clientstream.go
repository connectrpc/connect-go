// Package clientstream contains typed stream implementations from the caller's
// point of view.
package clientstream

import (
	"net/http"

	"github.com/bufbuild/connect"
)

// Client is the client's view of a client streaming RPC.
type Client[Req, Res any] struct {
	sender   connect.Sender
	receiver connect.Receiver
}

// NewClient constructs a Client.
func NewClient[Req, Res any](s connect.Sender, r connect.Receiver) *Client[Req, Res] {
	return &Client[Req, Res]{sender: s, receiver: r}
}

// RequestHeader returns the request headers. Headers are sent to the server with the
// first call to Send.
func (c *Client[Req, Res]) RequestHeader() http.Header {
	return c.sender.Header()
}

// Send a message to the server. The first call to Send also sends the request
// headers.
func (c *Client[Req, Res]) Send(msg *Req) error {
	return c.sender.Send(msg)
}

// CloseAndReceive closes the send side of the stream and waits for the
// response.
func (c *Client[Req, Res]) CloseAndReceive() (*connect.Response[Res], error) {
	if err := c.sender.Close(nil); err != nil {
		return nil, err
	}
	res, err := connect.ReceiveResponse[Res](c.receiver)
	if err != nil {
		_ = c.receiver.Close()
		return nil, err
	}
	if err := c.receiver.Close(); err != nil {
		return nil, err
	}
	return res, nil
}

// Server is the client's view of a server streaming RPC.
type Server[Res any] struct {
	receiver connect.Receiver
}

// NewServer constructs a Server.
func NewServer[Res any](r connect.Receiver) *Server[Res] {
	return &Server[Res]{receiver: r}
}

// Receive a message. When the server is done sending messages, Receive will
// return an error that wraps io.EOF.
func (s *Server[Res]) Receive() (*Res, error) {
	var res Res
	if err := s.receiver.Receive(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ResponseHeader returns the headers received from the server. It blocks until
// the first call to Receive returns.
func (s *Server[Res]) ResponseHeader() http.Header {
	return s.receiver.Header()
}

// ResponseTrailer returns the trailers received from the server. Trailers
// aren't fully populated until Receive() returns an error wrapping io.EOF.
func (s *Server[Res]) ResponseTrailer() http.Header {
	return s.receiver.Trailer()
}

// Close the receive side of the stream.
func (s *Server[Res]) Close() error {
	return s.receiver.Close()
}

// Bidirectional is the client's view of a bidirectional streaming RPC.
type Bidirectional[Req, Res any] struct {
	sender   connect.Sender
	receiver connect.Receiver
}

// NewBidirectional constructs a Bidirectional.
func NewBidirectional[Req, Res any](s connect.Sender, r connect.Receiver) *Bidirectional[Req, Res] {
	return &Bidirectional[Req, Res]{sender: s, receiver: r}
}

// RequestHeader returns the request headers. Headers are sent with the first
// call to Send.
func (b *Bidirectional[Req, Res]) RequestHeader() http.Header {
	return b.sender.Header()
}

// Send a message to the server. The first call to Send also sends the request
// headers.
func (b *Bidirectional[Req, Res]) Send(msg *Req) error {
	return b.sender.Send(msg)
}

// CloseSend closes the send side of the stream.
func (b *Bidirectional[Req, Res]) CloseSend() error {
	return b.sender.Close(nil)
}

// Receive a message. When the server is done sending messages, Receive will
// return an error that wraps io.EOF.
func (b *Bidirectional[Req, Res]) Receive() (*Res, error) {
	var res Res
	if err := b.receiver.Receive(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

// CloseReceive closes the receive side of the stream.
func (b *Bidirectional[Req, Res]) CloseReceive() error {
	return b.receiver.Close()
}

// ResponseHeader returns the headers received from the server. It blocks until
// the first call to Receive returns.
func (b *Bidirectional[Req, Res]) ResponseHeader() http.Header {
	return b.receiver.Header()
}

// ResponseTrailer returns the trailers received from the server. Trailers
// aren't fully populated until Receive() returns an error wrapping io.EOF.
func (b *Bidirectional[Req, Res]) ResponseTrailer() http.Header {
	return b.receiver.Trailer()
}
