package connect

import (
	"context"
	"errors"
	"io"
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
// errors can be cast to *Error using errors.As. The Close method of Sender
// implementations provided by this package automatically adds the appropriate
// codes when passed context.DeadlineExceeded or context.Canceled.
//
// Like the standard library's http.ResponseWriter, both client- and
// handler-side Senders write headers to the network with the first call to
// Send. Any subsequent mutations to the headers are effectively no-ops.
//
// Handler-side Senders may mutate trailers until calling Close, when the
// trailers are written to the network. Clients should avoid sending trailers:
// usage is nuanced and protocol-specific. For gRPC's HTTP/2 variant in
// particular, clients must set trailer keys prior to the first call to Send
// and then set trailer values before calling Close. See net/http's
// Request.Trailer for details.
type Sender interface {
	Send(any) error
	Close(error) error

	Spec() Specification
	Header() http.Header
	Trailer() http.Header
}

// Receiver is the readable side of a bidirectional stream of messages.
// Receiver implementations do not need to be safe for concurrent use.
//
// Receiver implementations provided by this module guarantee that all returned
// errors can be cast to *Error using errors.As.
type Receiver interface {
	Receive(any) error
	Close() error

	Spec() Specification
	Header() http.Header
	// Trailers are populated only after Receive returns an error wrapping
	// io.EOF.
	Trailer() http.Header
}

// Envelope is a wrapper around a generated request or response message. It
// provides access to metadata like headers, trailers, and the RPC
// specification, as well as strongly-typed access to the message itself.
type Envelope[T any] struct {
	Msg *T

	spec    Specification
	header  http.Header
	trailer http.Header
}

// NewEnvelope envelopes a request or response message.
func NewEnvelope[T any](message *T) *Envelope[T] {
	return &Envelope[T]{
		Msg: message,
		// Initialized lazily so we don't allocate unnecessarily.
		header:  nil,
		trailer: nil,
	}
}

// ReceiveUnaryEnvelope unmarshals a message from a Receiver, then envelopes
// the message and attaches the Receiver's headers, trailers, and RPC
// specification. It attempts to consume the Receiver and isn't appropriate
// when receiving multiple messages.
func ReceiveUnaryEnvelope[T any](receiver Receiver) (*Envelope[T], error) {
	var msg T
	if err := receiver.Receive(&msg); err != nil {
		return nil, err
	}
	// In a well-formed stream, the request message may be followed by a block
	// of in-stream trailers. To ensure that we receive the trailers, try to
	// read another message from the stream.
	if err := receiver.Receive(new(T)); err == nil {
		return nil, NewError(CodeUnknown, errors.New("unary stream has multiple messages"))
	} else if err != nil && !errors.Is(err, io.EOF) {
		return nil, NewError(CodeUnknown, err)
	}
	return &Envelope[T]{
		Msg:     &msg,
		spec:    receiver.Spec(),
		header:  receiver.Header(),
		trailer: receiver.Trailer(),
	}, nil
}

func receiveUnaryEnvelopeMetadata[T any](r Receiver) *Envelope[T] {
	return &Envelope[T]{
		Msg:     new(T),
		spec:    r.Spec(),
		header:  r.Header(),
		trailer: r.Trailer(),
	}
}

// Any returns the concrete request message as an empty interface, so that
// *Request implements the AnyRequest interface.
func (e *Envelope[_]) Any() any {
	return e.Msg
}

// Spec returns the Specification for this RPC.
func (e *Envelope[_]) Spec() Specification {
	return e.spec
}

// Header returns the HTTP headers for this request.
func (e *Envelope[_]) Header() http.Header {
	if e.header == nil {
		e.header = make(http.Header)
	}
	return e.header
}

// Trailer returns the trailers for this request. Depending on the underlying
// RPC protocol, trailers may be HTTP trailers, a protocol-specific block of
// metadata, or the union of the two.
func (e *Envelope[_]) Trailer() http.Header {
	if e.trailer == nil {
		e.trailer = make(http.Header)
	}
	return e.trailer
}

// internalOnly implements AnyEnvelope.
func (e *Envelope[_]) internalOnly() {}

// AnyEnvelope is the common method set of all Envelopes, regardless of type
// parameter. It's used in unary interceptors.
//
// To preserve our ability to add methods to this interface without breaking
// backward compatibility, only types defined in this package can implement
// AnyEnvelope.
type AnyEnvelope interface {
	Any() any
	Spec() Specification
	Header() http.Header
	Trailer() http.Header

	// Only internal implementations, so we can add methods without breaking
	// backward compatibility.
	internalOnly()
}

// Func is the generic signature of a unary RPC. Interceptors wrap Funcs.
//
// The type of the request and response struct depend on the codec being used.
// When using protobuf, they'll always be proto.Message implementations.
type Func func(context.Context, AnyEnvelope) (AnyEnvelope, error)

// An Interceptor adds logic to a generated handler or client, like the
// decorators or middleware you may have seen in other libraries. Interceptors
// may replace the context, mutate the request, mutate the response, handle the
// returned error, retry, recover from panics, emit logs and metrics, or do
// nearly anything else.
type Interceptor interface {
	// WrapUnary adds logic to a unary procedure. The returned Func must be safe
	// to call concurrently.
	WrapUnary(Func) Func

	// WrapStreamContext, WrapStreamSender, and WrapStreamReceiver work together
	// to add logic to streaming procedures. Stream interceptors work in phases.
	// First, each interceptor may wrap the request context. Then, the connect
	// runtime constructs a (Sender, Receiver) pair. Finally, each interceptor
	// may wrap the Sender and/or Receiver. For example, the flow within a
	// Handler looks like this:
	//
	//   func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	//     ctx := r.Context()
	//     if ic := h.interceptor; ic != nil {
	//       ctx = ic.WrapStreamContext(ctx)
	//     }
	//     sender, receiver := h.newStream(w, r.WithContext(ctx))
	//     if ic := h.interceptor; ic != nil {
	//       sender = ic.WrapStreamSender(ctx, sender)
	//       receiver = ic.WrapStreamReceiver(ctx, receiver)
	//     }
	//     h.serveStream(sender, receiver)
	//   }
	//
	// Sender and Receiver implementations don't need to be safe for concurrent
	// use.
	WrapStreamContext(context.Context) context.Context
	WrapStreamSender(context.Context, Sender) Sender
	WrapStreamReceiver(context.Context, Receiver) Receiver
}

// Doer is the transport-level interface connect expects HTTP clients to
// implement. The standard library's http.Client implements Doer.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Specification is a description of a client call or a handler invocation.
type Specification struct {
	StreamType StreamType
	Procedure  string // e.g., "acme.foo.v1.FooService/Bar"
	IsClient   bool   // otherwise we're in a handler
}
