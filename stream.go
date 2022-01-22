package rerpc

import (
	"net/http"
)

// Request is a request message and a variety of metadata (including headers).
type Request[Req any] struct {
	Msg *Req

	spec Specification
	hdr  Header
}

// NewRequest constructs a Request.
func NewRequest[Req any](msg *Req) *Request[Req] {
	return &Request[Req]{
		Msg: msg,
		hdr: Header{raw: make(http.Header)},
	}
}

// ReceiveRequest unmarshals a Request from a Receiver, then attaches the
// Receiver's headers and RPC specification.
func ReceiveRequest[Req any](r Receiver) (*Request[Req], error) {
	var msg Req
	if err := r.Receive(&msg); err != nil {
		return nil, err
	}
	return &Request[Req]{
		Msg:  &msg,
		spec: r.Spec(),
		hdr:  r.Header(),
	}, nil
}

// Any returns the concrete request message as an empty interface, so that
// *Request implements the AnyRequest interface. It should only be used in
// interceptors.
func (r *Request[_]) Any() any {
	return r.Msg
}

// Spec returns the Specification for this RPC.
func (r *Request[_]) Spec() Specification {
	return r.spec
}

// Header returns the HTTP headers for this request.
func (r *Request[_]) Header() Header {
	return r.hdr
}

// internalOnly implements AnyRequest.
func (r *Request[_]) internalOnly() {}

// Response is a response message plus a variety of metadata.
type Response[Res any] struct {
	Msg *Res

	hdr Header
}

// NewResponse constructs a Response.
func NewResponse[Res any](msg *Res) *Response[Res] {
	return &Response[Res]{
		Msg: msg,
		hdr: Header{raw: make(http.Header)},
	}
}

// ReceiveResponse unmarshals a Response from a Receiver, then attaches the
// Receiver's headers.
func ReceiveResponse[Res any](r Receiver) (*Response[Res], error) {
	var msg Res
	if err := r.Receive(&msg); err != nil {
		return nil, err
	}
	return &Response[Res]{
		Msg: &msg,
		hdr: r.Header(),
	}, nil
}

// Any returns the concrete request message as an empty interface, so that
// *Response implements the AnyResponse interface. It should only be used in
// interceptors.
func (r *Response[_]) Any() any {
	return r.Msg
}

// Header returns the HTTP headers for this response.
func (r *Response[_]) Header() Header {
	return r.hdr
}

// internalOnly implements AnyResponse.
func (r *Response[_]) internalOnly() {}

// Sender is the writable side of a bidirectional stream of messages.
type Sender interface {
	Send(any) error
	Close(error) error

	Spec() Specification
	Header() Header
}

// Receiver is the readable side of a bidirectional stream of messages.
type Receiver interface {
	Receive(any) error
	Close() error

	Spec() Specification
	Header() Header
}

type nopSender struct {
	spec   Specification
	header Header
}

var _ Sender = (*nopSender)(nil)

func newNopSender(spec Specification, header Header) *nopSender {
	return &nopSender{
		spec:   spec,
		header: header,
	}
}

func (n *nopSender) Header() Header {
	return n.header
}

func (n *nopSender) Spec() Specification {
	return n.spec
}

func (n *nopSender) Send(_ any) error {
	return nil
}

func (n *nopSender) Close(_ error) error {
	return nil
}

type nopReceiver struct {
	spec   Specification
	header Header
}

var _ Receiver = (*nopReceiver)(nil)

func newNopReceiver(spec Specification, header Header) *nopReceiver {
	return &nopReceiver{
		spec:   spec,
		header: header,
	}
}

func (n *nopReceiver) Spec() Specification {
	return n.spec
}

func (n *nopReceiver) Header() Header {
	return n.header
}

func (n *nopReceiver) Receive(_ any) error {
	return nil
}

func (n *nopReceiver) Close() error {
	return nil
}
