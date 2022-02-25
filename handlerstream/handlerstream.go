// Package handlerstream contains typed stream implementations from the server's
// point of view.
// See comments on clientstream
package handlerstream

import (
	"net/http"

	"github.com/bufbuild/connect"
)

// Client is the server's view of a client streaming RPC.
type Client[Req, Res any] struct {
	sender   connect.Sender
	receiver connect.Receiver
}

// NewClient constructs a Client.
func NewClient[Req, Res any](s connect.Sender, r connect.Receiver) *Client[Req, Res] {
	return &Client[Req, Res]{sender: s, receiver: r}
}

// RequestHeader returns the headers received from the client.
func (c *Client[Req, Res]) RequestHeader() http.Header {
	return c.receiver.Header()
}

// Receive a message. When the client is done sending messages, Receive returns
// an error that wraps io.EOF.
func (c *Client[Req, Res]) Receive() (*Req, error) {
	var req Req
	if err := c.receiver.Receive(&req); err != nil {
		return nil, err
	}
	return &req, nil
}

// SendAndClose closes the receive side of the stream, then sends a response
// back to the client.
func (c *Client[Req, Res]) SendAndClose(msg *connect.Response[Res]) error {
	if err := c.receiver.Close(); err != nil {
		return err
	}
	sendHeader := c.sender.Header()
	for k, v := range msg.Header() {
		sendHeader[k] = append(sendHeader[k], v...)
	}
	sendTrailer := c.sender.Trailer()
	for k, v := range msg.Trailer() {
		sendTrailer[k] = append(sendTrailer[k], v...)
	}
	return c.sender.Send(msg.Msg)
}

// Server is the server's view of a server streaming RPC.
type Server[Res any] struct {
	sender connect.Sender
}

// NewServer constructs a Server.
func NewServer[Res any](s connect.Sender) *Server[Res] {
	return &Server[Res]{sender: s}
}

// ResponseHeader returns the response headers. Headers are sent with the first
// call to Send.
func (s *Server[Res]) ResponseHeader() http.Header {
	return s.sender.Header()
}

// ResponseTrailer returns the response trailers. Handlers may write to the
// response trailers at any time before returning.
func (s *Server[Res]) ResponseTrailer() http.Header {
	return s.sender.Trailer()
}

// Send a message to the client. The first call to Send also sends the response
// headers.
func (s *Server[Res]) Send(msg *Res) error {
	return s.sender.Send(msg)
}

// Bidirectional is the server's view of a bidirectional streaming RPC.
type Bidirectional[Req, Res any] struct {
	sender   connect.Sender
	receiver connect.Receiver
}

// NewBidirectional constructs a Bidirectional.
func NewBidirectional[Req, Res any](s connect.Sender, r connect.Receiver) *Bidirectional[Req, Res] {
	return &Bidirectional[Req, Res]{sender: s, receiver: r}
}

// RequestHeader returns the headers received from the client.
func (b *Bidirectional[Req, Res]) RequestHeader() http.Header {
	return b.receiver.Header()
}

// Receive a message. When the client is done sending messages, Receive will
// return an error that wraps io.EOF.
func (b *Bidirectional[Req, Res]) Receive() (*Req, error) {
	var req Req
	if err := b.receiver.Receive(&req); err != nil {
		return nil, err
	}
	return &req, nil
}

// ResponseHeader returns the response headers. Headers are sent with the first
// call to Send.
func (b *Bidirectional[Req, Res]) ResponseHeader() http.Header {
	return b.sender.Header()
}

// ResponseTrailer returns the response trailers. Handlers may write to the
// response trailers at any time before returning.
func (b *Bidirectional[Req, Res]) ResponseTrailer() http.Header {
	return b.sender.Trailer()
}

func (b *Bidirectional[Req, Res]) Send(msg *Res) error {
	return b.sender.Send(msg)
}
