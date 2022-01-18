package rerpc

import (
	"net/http"
)

// Request is a request message and a variety of metadata.
type Request[Req any] struct {
	Msg *Req

	spec Specification
	hdr  Header
}

func NewRequest[Req any](msg *Req) *Request[Req] {
	return &Request[Req]{
		Msg: msg,
		hdr: Header{raw: make(http.Header)},
	}
}

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

func (r *Request[_]) Any() any {
	return r.Msg
}

func (r *Request[_]) Spec() Specification {
	return r.spec
}

func (r *Request[_]) Header() Header {
	return r.hdr
}

func (r *Request[_]) internalOnly() {}

type Response[Res any] struct {
	Msg *Res

	hdr Header
}

func NewResponse[Res any](msg *Res) *Response[Res] {
	return &Response[Res]{
		Msg: msg,
		hdr: Header{raw: make(http.Header)},
	}
}

func newResponseWithHeader[Res any](msg *Res, header Header) *Response[Res] {
	return &Response[Res]{
		Msg: msg,
		hdr: header,
	}
}

func (r *Response[_]) Any() any {
	return r.Msg
}

func (r *Response[_]) Header() Header {
	return r.hdr
}

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
