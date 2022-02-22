package connect

import (
	"net/http"
)

type nopSender struct {
	spec    Specification
	header  http.Header
	trailer http.Header
}

var _ Sender = (*nopSender)(nil)

func newNopSender(spec Specification, header, trailer http.Header) *nopSender {
	return &nopSender{
		spec:    spec,
		header:  header,
		trailer: trailer,
	}
}

func (n *nopSender) Header() http.Header {
	return n.header
}

func (n *nopSender) Trailer() http.Header {
	return n.trailer
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
	spec    Specification
	header  http.Header
	trailer http.Header
}

var _ Receiver = (*nopReceiver)(nil)

func newNopReceiver(spec Specification, header, trailer http.Header) *nopReceiver {
	return &nopReceiver{
		spec:    spec,
		header:  header,
		trailer: trailer,
	}
}

func (n *nopReceiver) Spec() Specification {
	return n.spec
}

func (n *nopReceiver) Header() http.Header {
	return n.header
}

func (n *nopReceiver) Trailer() http.Header {
	return n.trailer
}

func (n *nopReceiver) Receive(_ any) error {
	return nil
}

func (n *nopReceiver) Close() error {
	return nil
}
