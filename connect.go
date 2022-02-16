package connect

import (
	"context"
	"net/http"
)

// Version is the semantic version of the connect module.
const Version = "0.0.1"

// These constants are used in compile-time handshakes with connect's generated
// code.
const IsAtLeastVersion0_0_1 = true

// StreamType describes whether the client, server, neither, or both is
// streaming.
type StreamType uint8

const (
	StreamTypeUnary         StreamType = 0b00
	StreamTypeClient                   = 0b01
	StreamTypeServer                   = 0b10
	StreamTypeBidirectional            = StreamTypeClient | StreamTypeServer
)

// Sender is the writable side of a bidirectional stream of messages. Sender
// implementations do not need to be safe for concurrent use.
//
// Sender implementations provided by this module guarantee that all returned
// errors are *Errors, with codes. The Close method of implementations provided
// by this package automatically adds the appropriate codes when passed
// context.DeadlineExceeded or context.Canceled.
type Sender interface {
	Send(any) error
	Close(error) error

	Spec() Specification
	Header() http.Header
}

// Receiver is the readable side of a bidirectional stream of messages.
// Receiver implementations do not need to be safe for concurrent use.
//
// Receiver implementations provided by this module guarantee that all returned
// errors are *Errors, with codes.
type Receiver interface {
	Receive(any) error
	Close() error

	Spec() Specification
	Header() http.Header
}

// Request is a request message and metadata (including headers).
type Request[Req any] struct {
	Msg *Req

	spec Specification
	hdr  http.Header
}

// NewRequest constructs a Request.
func NewRequest[Req any](msg *Req) *Request[Req] {
	return &Request[Req]{
		Msg: msg,
		hdr: make(http.Header),
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

func receiveRequestMetadata[Req any](r Receiver) *Request[Req] {
	var msg Req
	return &Request[Req]{
		Msg:  &msg,
		spec: r.Spec(),
		hdr:  r.Header(),
	}
}

// Any returns the concrete request message as an empty interface, so that
// *Request implements the AnyRequest interface.
func (r *Request[_]) Any() any {
	return r.Msg
}

// Spec returns the Specification for this RPC.
func (r *Request[_]) Spec() Specification {
	return r.spec
}

// Header returns the HTTP headers for this request.
func (r *Request[_]) Header() http.Header {
	return r.hdr
}

// internalOnly implements AnyRequest.
func (r *Request[_]) internalOnly() {}

// Response is a response message and metadata.
type Response[Res any] struct {
	Msg *Res

	hdr http.Header
}

// NewResponse constructs a Response.
func NewResponse[Res any](msg *Res) *Response[Res] {
	return &Response[Res]{
		Msg: msg,
		hdr: make(http.Header),
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
func (r *Response[_]) Header() http.Header {
	return r.hdr
}

// internalOnly implements AnyResponse.
func (r *Response[_]) internalOnly() {}

// AnyRequest is the common method set of all Requests, regardless of message
// type. It's used in unary interceptors.
//
// To preserve our ability to add methods to this interface without breaking
// backward compatibility, only types defined in this package can implement
// AnyRequest.
type AnyRequest interface {
	Any() any
	Spec() Specification
	Header() http.Header

	// Only internal implementations, so we can add methods without breaking
	// backward compatibility.
	internalOnly()
}

// AnyResponse is the common method set of all Responses, regardless of message
// type. It's used in unary interceptors.
//
// To preserve our ability to add methods to this interface without breaking
// backward compatibility, only types defined in this package can implement
// AnyResponse.
type AnyResponse interface {
	Any() any
	Header() http.Header

	// Only internal implementations, so we can add methods without breaking
	// backward compatibility.
	internalOnly()
}

// Func is the generic signature of a unary RPC. Interceptors wrap Funcs.
//
// The type of the request and response struct depend on the codec being used.
// When using protobuf, they'll always be proto.Message implementations.
type Func func(context.Context, AnyRequest) (AnyResponse, error)

// An Interceptor adds logic to a generated handler or client, like the
// decorators or middleware you may have seen in other libraries. Interceptors
// may replace the context, mutate the request, mutate the response, handle the
// returned error, retry, recover from panics, emit logs and metrics, or do
// nearly anything else.
type Interceptor interface {
	// Wrap adds logic to a unary procedure. The returned Func must be safe to
	// call concurrently.
	Wrap(Func) Func

	// WrapContext, WrapSender, and WrapReceiver work together to add logic to
	// streaming procedures. Stream interceptors work in phases. First, each
	// interceptor may wrap the request context. Then, the connect runtime
	// constructs a (Sender, Receiver) pair. Finally, each interceptor may wrap
	// the Sender and/or Receiver. For example, the flow within a Handler looks
	// like this:
	//
	//   func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	//     ctx := r.Context()
	//     if ic := h.interceptor; ic != nil {
	//       ctx = ic.WrapContext(ctx)
	//     }
	//     sender, receiver := h.newStream(w, r.WithContext(ctx))
	//     if ic := h.interceptor; ic != nil {
	//       sender = ic.WrapSender(ctx, sender)
	//       receiver = ic.WrapReceiver(ctx, receiver)
	//     }
	//     h.serveStream(sender, receiver)
	//   }
	//
	// Sender and Receiver implementations don't need to be safe for concurrent
	// use.
	WrapContext(context.Context) context.Context
	WrapSender(context.Context, Sender) Sender
	WrapReceiver(context.Context, Receiver) Receiver
}

// Doer is the transport-level interface connect expects HTTP clients to
// implement. The standard library's http.Client implements Doer.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Specification is a description of a client call or a handler invocation.
type Specification struct {
	Type      StreamType
	Procedure string // e.g., "acme.foo.v1.FooService/Bar"
	IsClient  bool   // otherwise we're in a handler
}
