// Copyright 2021-2026 The Connect Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package connect is a slim RPC framework built on Protocol Buffers and
// [net/http]. In addition to supporting its own protocol, Connect handlers and
// clients are wire-compatible with gRPC and gRPC-Web, including streaming.
//
// This documentation is intended to explain each type and function in
// isolation. Walkthroughs, FAQs, and other narrative docs are available on the
// [Connect website], and there's a working [demonstration service] on Github.
//
// [Connect website]: https://connectrpc.com
// [demonstration service]: https://github.com/connectrpc/examples-go
package connect

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
)

// Version is the semantic version of the connect module.
const Version = "2.0.0-dev"

// Well-known codec names.
const (
	// CodecNameProto is the protocol token for protobuf binary encoding.
	CodecNameProto = "proto"
	// CodecNameJSON is the protocol token for protobuf JSON encoding.
	CodecNameJSON = "json"
)

// Well-known compression names.
const (
	// CompressionNameIdentity is the token for uncompressed messages. It is
	// named so transport metadata can report "identity" consistently even
	// though no compressor is needed.
	CompressionNameIdentity = "identity"
	// CompressionNameGzip is the token for gzip compression.
	CompressionNameGzip = "gzip"
	// CompressionNameBr is the token for Brotli compression. The core package
	// defines the name so transports and compressor packages can agree on the
	// token. It does not provide a Brotli compressor.
	CompressionNameBr = "br"
	// CompressionNameZstd is the token for Zstandard compression. The core
	// package defines the name so transports and compressor packages can agree
	// on the token. It does not provide a Zstandard compressor.
	CompressionNameZstd = "zstd"
)

// Well-known protocol names reported by [CallInfo.Protocol].
const (
	// ProtocolNameConnect is the token for the Connect protocol.
	ProtocolNameConnect = "connect"
	// ProtocolNameGRPC is the token for the gRPC protocol.
	ProtocolNameGRPC = "grpc"
	// ProtocolNameGRPCWeb is the token for the gRPC-Web protocol.
	ProtocolNameGRPCWeb = "grpcweb"
)

// StreamType values describe the directionality of an RPC.
const (
	// StreamTypeUnary identifies an RPC with one request and one response.
	StreamTypeUnary StreamType = 0b00
	// StreamTypeClient identifies a client-streaming RPC.
	StreamTypeClient StreamType = 0b01
	// StreamTypeServer identifies a server-streaming RPC.
	StreamTypeServer StreamType = 0b10
	// StreamTypeBidi identifies a bidirectional-streaming RPC.
	StreamTypeBidi = StreamTypeClient | StreamTypeServer
)

// IdempotencyLevel values mirror protobuf MethodOptions.idempotency_level.
const (
	// IdempotencyUnknown means the schema does not declare an idempotency
	// level.
	IdempotencyUnknown IdempotencyLevel = 0
	// IdempotencyNoSideEffects means the RPC has no side effects. Transports
	// may use this to enable protocol features such as Connect GET.
	IdempotencyNoSideEffects IdempotencyLevel = 1
	// IdempotencyIdempotent means repeated identical requests have the same
	// effect as one request.
	IdempotencyIdempotent IdempotencyLevel = 2
)

// Standard Connect RPC codes. [Code.String] returns the lowercase protocol
// token for each code.
const (
	// The zero code in gRPC is OK, which indicates that the operation was a
	// success. We don't define a constant for it because it overlaps awkwardly
	// with Go's error semantics: what does it mean to have a non-nil error with
	// an OK status? (Also, the Connect protocol doesn't use a code for
	// successes.)

	// CodeCanceled indicates that the operation was canceled, typically by the
	// caller.
	CodeCanceled Code = 1

	// CodeUnknown indicates that the operation failed for an unknown reason.
	CodeUnknown Code = 2

	// CodeInvalidArgument indicates that client supplied an invalid argument.
	CodeInvalidArgument Code = 3

	// CodeDeadlineExceeded indicates that deadline expired before the operation
	// could complete.
	CodeDeadlineExceeded Code = 4

	// CodeNotFound indicates that some requested entity (for example, a file or
	// directory) was not found.
	CodeNotFound Code = 5

	// CodeAlreadyExists indicates that client attempted to create an entity (for
	// example, a file or directory) that already exists.
	CodeAlreadyExists Code = 6

	// CodePermissionDenied indicates that the caller doesn't have permission to
	// execute the specified operation.
	CodePermissionDenied Code = 7

	// CodeResourceExhausted indicates that some resource has been exhausted. For
	// example, a per-user quota may be exhausted or the entire file system may
	// be full.
	CodeResourceExhausted Code = 8

	// CodeFailedPrecondition indicates that the system is not in a state
	// required for the operation's execution.
	CodeFailedPrecondition Code = 9

	// CodeAborted indicates that operation was aborted by the system, usually
	// because of a concurrency issue such as a sequencer check failure or
	// transaction abort.
	CodeAborted Code = 10

	// CodeOutOfRange indicates that the operation was attempted past the valid
	// range (for example, seeking past end-of-file).
	CodeOutOfRange Code = 11

	// CodeUnimplemented indicates that the operation isn't implemented,
	// supported, or enabled in this service.
	CodeUnimplemented Code = 12

	// CodeInternal indicates that some invariants expected by the underlying
	// system have been broken. This code is reserved for serious errors.
	CodeInternal Code = 13

	// CodeUnavailable indicates that the service is currently unavailable. This
	// is usually temporary, so clients can back off and retry idempotent
	// operations.
	CodeUnavailable Code = 14

	// CodeDataLoss indicates that the operation has resulted in unrecoverable
	// data loss or corruption.
	CodeDataLoss Code = 15

	// CodeUnauthenticated indicates that the request does not have valid
	// authentication credentials for the operation.
	CodeUnauthenticated Code = 16

	minCode = CodeCanceled
	maxCode = CodeUnauthenticated
)

// Transport is the boundary between [Client] and an RPC execution environment.
// A Client passes the RPC [Spec] to a Transport, then drives the returned
// [ClientStream] by sending request messages, closing the send side, receiving
// response messages, and closing the stream. Generated clients hold a [*Client]
// rather than a Transport directly.
//
// [connectrpc.com/connect/v2/connecthttp.NewTransport] returns a Transport that
// sends RPCs over Connect, gRPC, or gRPC-Web on net/http. Other implementations
// can dispatch directly to a [Server], use a test double, or adapt another wire
// protocol without regenerating clients.
type Transport interface {
	// NewClientStream opens a client stream for spec.
	NewClientStream(ctx context.Context, spec Spec) (ClientStream, error)
}

// Client bundles a [Transport] with the client-side interceptor chain.
// Generated service clients hold a [*Client] and dispatch every RPC through
// one of the Call methods.
//
// The interceptor chain is applied once in [NewClient], producing one
// prebuilt [ClientFunc].
type Client struct {
	transport  Transport
	clientFunc ClientFunc
}

// NewClient returns a [Client] that dispatches RPCs over transport. The
// interceptors fire in argument order. The first wraps the outermost call.
// Interceptors that need per-message behavior wrap the [ClientStream] that
// next returns.
//
// The interceptor chain is applied here, once, not on every RPC. Per-RPC
// state belongs in the [ClientFunc] an interceptor returns, or in a
// [ClientStream] wrapper, not in the interceptor function itself.
func NewClient(transport Transport, interceptors ...ClientInterceptor) *Client {
	return &Client{
		transport:  transport,
		clientFunc: chainClientInterceptors(interceptors, transport.NewClientStream),
	}
}

// CallUnary opens a stream for spec, sends req, closes the send side,
// and reads a single response into res.
//
// The initialization passes through the interceptor chain. The full
// [ClientStream.Send], [ClientStream.CloseSend], and [ClientStream.Receive]
// sequence is then executed on the resulting stream. Receive reads the
// response to [io.EOF], which releases the stream's resources.
func (c *Client) CallUnary(ctx context.Context, spec Spec, req, res any) (err error) {
	ctx = ensureClientContext(ctx)
	stream, err := c.clientFunc(ctx, spec)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, stream.Close())
	}()
	if err := stream.Send(req); err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	return stream.Receive(res)
}

// CallClientStream opens a stream for spec for client-streaming or bidi RPCs.
// The returned [ClientStream] is wrapped by interceptors when any are
// configured.
//
// The stream's underlying resources are released automatically when
// [ClientStream.Receive] returns [io.EOF] or the RPC context is canceled. To
// abandon a stream before reading to completion, cancel the context or call
// [ClientStream.Close].
func (c *Client) CallClientStream(ctx context.Context, spec Spec) (ClientStream, error) {
	ctx = ensureClientContext(ctx)
	return c.clientFunc(ctx, spec)
}

// CallServerStream opens a stream for spec, sends the single request, closes
// the send side, and returns the stream for the caller to read responses from.
// The returned [ClientStream] is wrapped by interceptors when any are
// configured.
//
// Its resources are released automatically when [ClientStream.Receive]
// returns [io.EOF] or the RPC context is canceled. To abandon the stream
// early, cancel the context or call [ClientStream.Close].
func (c *Client) CallServerStream(ctx context.Context, spec Spec, req any) (ClientStream, error) {
	ctx = ensureClientContext(ctx)
	stream, err := c.clientFunc(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(req); err != nil {
		return nil, err
	}
	if err := stream.CloseSend(); err != nil {
		return nil, err
	}
	return stream, nil
}

// Server is the procedure-to-method dispatcher. It owns the method
// registry and the server-side interceptor chain.
type Server struct {
	chain   ServerInterceptor
	methods map[string]Method // procedure -> registered method
	specs   []Spec
	unknown ServerFunc // fallback for unregistered procedures, nil means CodeUnimplemented
}

// NewServer returns a dispatcher with no registered methods. The interceptors
// fire in argument order. The first wraps the outermost call. Interceptors
// that need per-message behavior should wrap the [ServerStream] before
// calling next.
func NewServer(interceptors ...ServerInterceptor) *Server {
	return &Server{
		chain:   chainServerInterceptors(interceptors),
		methods: map[string]Method{},
	}
}

// Register stores each method on the dispatcher. Each [Method.Handler] is
// wrapped with the server-side interceptor chain so [Server.Call] is just a map
// lookup plus one call.
//
// Register panics if two methods have the same [Spec.Procedure]. Register
// is intended for setup and must not run concurrently with [Server.Register],
// [Server.Specs], or [Server.Call].
func (s *Server) Register(methods ...Method) {
	for _, method := range methods {
		if _, dup := s.methods[method.Spec.Procedure]; dup {
			panic(fmt.Sprintf("connect: duplicate procedure %q", method.Spec.Procedure)) //nolint:forbidigo // setup-time misuse: panic surfaces the bug at startup
		}
		if s.chain != nil {
			method.Handler = s.chain(method.Handler)
		}
		s.methods[method.Spec.Procedure] = method
		s.specs = append(s.specs, method.Spec)
	}
}

// Specs yields the [Spec] values of registered methods. Transports use this
// to install per-procedure routes. Specs are yielded in registration order.
// Do not call Specs concurrently with [Server.Register].
func (s *Server) Specs() iter.Seq[Spec] {
	return func(yield func(Spec) bool) {
		for _, spec := range s.specs {
			if !yield(spec) {
				return
			}
		}
	}
}

// Call dispatches an RPC to the method registered for procedure. Transports
// build the [ServerStream] from their wire input, pass the per-RPC [CallInfo]
// (or nil to let Call allocate one), and Call attaches it to ctx so user
// handlers can read it via [CallInfoForServerContext]. Call returns the method
// error for the transport to encode in its protocol. It does not finalize
// protocol state. The transport does that once Call returns. Returns
// [CodeUnimplemented] when no method is registered.
func (s *Server) Call(ctx context.Context, procedure string, info *CallInfo, stream ServerStream) error {
	if info == nil {
		info = &CallInfo{}
	}
	ctx = withServerContext(ctx, info)
	ctx = clearClientContext(ctx)
	method, ok := s.methods[procedure]
	if !ok {
		if s.unknown != nil {
			return s.unknown(ctx, Spec{Procedure: procedure, StreamType: StreamTypeBidi}, stream)
		}
		return Errorf(CodeUnimplemented, "procedure %q not registered", procedure)
	}
	return method.Handler(ctx, method.Spec, stream)
}

// SetUnknownHandler sets the fallback [ServerFunc] that [Server.Call] invokes when
// no method is registered for a procedure. The handler receives a [Spec] whose
// Procedure is the requested procedure and whose StreamType is [StreamTypeBidi],
// the most permissive shape, since the cardinality of an unregistered
// procedure is unknown. Its Schema is nil. The handler is wrapped by the
// server interceptor chain like a registered method. A nil fn restores the
// default, which fails the call with [CodeUnimplemented].
//
// SetUnknownHandler is intended for setup and must not run concurrently with
// [Server.Call] or [Server.Register].
func (s *Server) SetUnknownHandler(fn ServerFunc) {
	if fn != nil && s.chain != nil {
		fn = s.chain(fn)
	}
	s.unknown = fn
}

// Spec describes a single RPC procedure. Generated code typically provides
// one Spec per protobuf method.
type Spec struct {
	// StreamType describes which side, if any, sends multiple messages.
	// It controls which stream operations generated wrappers expose.
	StreamType StreamType
	// IdempotencyLevel mirrors protobuf MethodOptions.idempotency_level.
	// Transports may use it for protocol features such as Connect GET.
	IdempotencyLevel IdempotencyLevel
	// Schema is opaque method schema information. Protobuf generated code
	// stores a protoreflect.MethodDescriptor. Other schema systems may use
	// their own descriptor types.
	Schema any
	// Procedure is the leading-slash RPC path, such as
	// "/package.Service/Method". Server uses it as the registry key, and
	// HTTP transports use it as the route path.
	Procedure string
}

// ClientStream is the client-side view of a single in-flight RPC.
//
// A caller drives the stream by sending request messages, closing the send side
// with CloseSend, and receiving response messages until Receive reports the end
// of the response stream with an error matching io.EOF under [errors.Is].
// Reading to io.EOF releases the stream's resources and makes the response
// trailers available on the call's [CallInfo]. To abandon a stream before
// io.EOF, cancel the context passed to the [Client] call: that releases
// resources and unblocks any pending operation.
// [Client.CallUnary] drives this full sequence internally.
//
// The first Send or Receive flushes the request headers from
// [CallInfo.RequestHeader] and opens the stream. Call
// [ClientStream.SendHeaders] to flush them eagerly without sending a message.
// After the headers are flushed, later mutations to the request headers may
// not affect the request.
//
// Send transmits request messages, and Receive reads response messages. The
// msg passed to Send and Receive must match the message type described by the
// stream's [Spec]. Generated wrappers expose typed methods for the legal
// operations, but raw stream users must follow Spec.StreamType themselves.
//
// A stream supports one active send-side operation and one active receive-side
// operation at a time, and the two sides may run concurrently. Do not call
// Send concurrently with Send or CloseSend, and do not call Receive
// concurrently with Receive.
//
// RPC status and protocol errors returned by streams can be inspected with
// [errors.As] into [*Error] or classified with [CodeOf]. Clean receive-side
// completion is reported by an error that matches io.EOF under [errors.Is].
// Context cancellation may be reported directly as [context.Canceled] or
// [context.DeadlineExceeded], or as an [*Error] whose cause matches one of
// those errors.
//
// The stream does not expose [Spec] or [CallInfo]. The Spec is passed
// alongside the stream into [ClientFunc], and the CallInfo is reached through
// ctx via [CallInfoForClientContext].
type ClientStream interface {
	// SendHeaders flushes the request headers and opens the stream without
	// sending a request message. The first Send or Receive does this
	// implicitly; call SendHeaders only to flush eagerly, for example to let
	// the server begin work, surface connection errors before the first
	// message, or record timing. It is idempotent.
	SendHeaders() error
	// Send sends msg as the next request message.
	Send(msg any) error
	// CloseSend closes the request side of the stream. It is idempotent.
	CloseSend() error
	// Receive reads the next response message into msg. It reports clean
	// receive-side completion with an error that matches io.EOF under errors.Is.
	// Reading to io.EOF releases the stream's resources; to abandon a stream
	// earlier, cancel the call's context or call Close.
	Receive(msg any) error
	// Close releases the stream's resources, unblocking a pending Receive and
	// tearing the stream down. It may be called concurrently with Receive,
	// typically as defer stream.Close(). It is idempotent. After Close, the
	// stream must not be used again.
	Close() error
}

// ServerStream is the server-side view of a single in-flight RPC.
// The [Spec] is passed alongside the stream into [ServerFunc], and the
// [CallInfo] is reached through ctx via [CallInfoForServerContext].
//
// The first Send flushes the response headers from [CallInfo.ResponseHeader].
// Call [ServerStream.SendHeaders] to flush them eagerly without sending a
// response message. After the headers are flushed, later mutations to the
// response headers may not affect the response.
//
// Receive reads request messages, and Send transmits response messages. The
// msg passed to Receive and Send must match the message type described by the
// stream's [Spec]. Generated wrappers expose typed methods for the legal
// operations, but raw stream users must follow Spec.StreamType themselves.
//
// A stream supports one active send-side operation and one active receive-side
// operation at a time, and the two sides may run concurrently. Do not call Send
// concurrently with Send, and do not call Receive concurrently with Receive.
//
// The server stream has no Close method: the transport finalizes the RPC when
// the handler returns, encoding the handler's error and any trailers. Handler
// code ends the RPC by returning, not by closing the stream.
//
// RPC status and protocol errors returned by streams can be inspected with
// [errors.As] into [*Error] or classified with [CodeOf]. Clean receive-side
// completion is reported by an error that matches io.EOF under [errors.Is].
// Context cancellation may be reported directly as [context.Canceled] or
// [context.DeadlineExceeded], or as an [*Error] whose cause matches one of
// those errors.
type ServerStream interface {
	// Receive reads the next request message into msg. It reports clean
	// receive-side completion with an error that matches io.EOF under errors.Is.
	Receive(msg any) error
	// SendHeaders flushes the response headers and opens the stream without
	// sending a response message. The first Send does this implicitly; call
	// SendHeaders only to flush eagerly, for example to let the client begin
	// work or surface headers before the first Send. It is idempotent.
	SendHeaders() error
	// Send sends msg as the next response message.
	Send(msg any) error
}

// ClientFunc opens a [ClientStream] for an RPC. Interceptors wrap one
// ClientFunc to produce another. The innermost function invokes the
// [Transport] to establish the connection and initialize the stream.
type ClientFunc func(ctx context.Context, spec Spec) (ClientStream, error)

// ServerFunc serves an RPC on an open [ServerStream]. Interceptors wrap one
// ServerFunc to produce another. The innermost function is the registered
// [Method.Handler].
type ServerFunc func(ctx context.Context, spec Spec, stream ServerStream) error

// ClientInterceptor wraps a [ClientFunc]. It may return an error without
// calling next to short-circuit the RPC.
//
// Unlike server interceptors, client interceptors wrap the initialization
// of the stream rather than the full execution of the RPC. This model is
// identical across unary and streaming calls. Because next opens the stream,
// deriving a new context and passing it to next propagates that context to the
// [Transport] and the stream. To observe or modify messages, interceptors call
// next and wrap the returned [ClientStream]. To observe the end of the RPC,
// wrap both Close and Receive: streaming callers may finish at [io.EOF]
// without calling Close.
//
// [NewClient] applies the chain when the client is constructed, so the
// interceptor function runs once, not once per RPC. Keep per-RPC state in the
// returned ClientFunc or stream wrapper.
type ClientInterceptor func(next ClientFunc) ClientFunc

// ServerInterceptor wraps a [ServerFunc]. It may return an error without
// calling next to short-circuit the RPC.
//
// Server interceptors surround the full server invocation. To observe or
// modify individual messages, interceptors can pass a wrapped [ServerStream]
// to next.
type ServerInterceptor func(next ServerFunc) ServerFunc

// StreamType describes the directionality of an RPC.
type StreamType uint8

// String returns a human-readable stream type name.
func (s StreamType) String() string {
	switch s {
	case StreamTypeUnary:
		return "unary"
	case StreamTypeClient:
		return "client"
	case StreamTypeServer:
		return "server"
	case StreamTypeBidi:
		return "bidi"
	}
	return fmt.Sprintf("stream_%d", uint8(s))
}

// IdempotencyLevel mirrors the protobuf MethodOptions idempotency_level.
type IdempotencyLevel int32

// String returns a human-readable idempotency-level name.
func (i IdempotencyLevel) String() string {
	switch i {
	case IdempotencyUnknown:
		return "idempotency_unknown"
	case IdempotencyNoSideEffects:
		return "no_side_effects"
	case IdempotencyIdempotent:
		return "idempotent"
	}
	return fmt.Sprintf("idempotency_%d", int(i))
}

// Method binds a [Spec] to the [ServerFunc] that serves it.
type Method struct {
	// Spec describes the RPC procedure being registered.
	Spec Spec
	// Handler serves the RPC described by Spec.
	Handler ServerFunc
}

// Header is a mutable, case-insensitive, multi-valued string map, mirroring
// [net/http.Header]. The zero value is ready to use. Header is not safe for
// concurrent use.
//
// Keys are canonicalized with textproto.CanonicalMIMEHeaderKey. Transports
// ignore or overwrite protocol-reserved keys when sending an RPC.
type Header struct {
	store map[string][]string
}

// Get returns the first value associated with key, or "" if the key has no
// values.
func (m *Header) Get(key string) string {
	if m == nil {
		return ""
	}
	vs := m.store[textproto.CanonicalMIMEHeaderKey(key)]
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

func (m *Header) Has(key string) bool {
	if m == nil {
		return false
	}
	_, ok := m.store[textproto.CanonicalMIMEHeaderKey(key)]
	return ok
}

// Values returns all values associated with key. The returned slice aliases
// the header.
func (m *Header) Values(key string) []string {
	if m == nil {
		return nil
	}
	return m.store[textproto.CanonicalMIMEHeaderKey(key)]
}

// Set replaces key's values with value.
func (m *Header) Set(key, value string) {
	m.ensure()[textproto.CanonicalMIMEHeaderKey(key)] = []string{value}
}

// SetValues replaces key's values with values. It does not copy values.
func (m *Header) SetValues(key string, values []string) {
	canonical := textproto.CanonicalMIMEHeaderKey(key)
	m.ensure()[canonical] = values
}

// Add appends value to key's values.
func (m *Header) Add(key, value string) {
	k := textproto.CanonicalMIMEHeaderKey(key)
	m.ensure()[k] = append(m.store[k], value) //nolint:gocritic // ensure() returns m.store, so the LHS and RHS refer to the same slot
}

// Delete removes all values for key.
func (m *Header) Delete(key string) {
	if m == nil {
		return
	}
	delete(m.store, textproto.CanonicalMIMEHeaderKey(key))
}

// All yields each key and its values in unspecified order. The yielded slices
// alias the header.
func (m *Header) All() iter.Seq2[string, []string] {
	return func(yield func(string, []string) bool) {
		if m == nil {
			return
		}
		for k, v := range m.store {
			if !yield(k, v) {
				return
			}
		}
	}
}

// Len returns the number of keys.
func (m *Header) Len() int {
	if m == nil {
		return 0
	}
	return len(m.store)
}

func (m *Header) ensure() map[string][]string {
	if m.store == nil {
		m.store = make(map[string][]string)
	}
	return m.store
}

// EncodeBinaryHeader base64-encodes the data. It always emits unpadded values.
//
// In the Connect, gRPC, and gRPC-Web protocols, binary headers must have keys
// ending in "-Bin".
func EncodeBinaryHeader(data []byte) string {
	// gRPC specification says that implementations should emit unpadded values.
	return base64.RawStdEncoding.EncodeToString(data)
}

// DecodeBinaryHeader base64-decodes the data. It can decode padded or unpadded
// values. Following usual HTTP semantics, multiple base64-encoded values may
// be joined with a comma. When receiving such comma-separated values, split
// them with [strings.Split] before calling DecodeBinaryHeader.
//
// Binary headers sent using the Connect, gRPC, and gRPC-Web protocols have
// keys ending in "-Bin".
func DecodeBinaryHeader(data string) ([]byte, error) {
	if len(data)%4 != 0 {
		// Data definitely isn't padded.
		return base64.RawStdEncoding.DecodeString(data)
	}
	// Either the data was padded, or padding wasn't necessary. In both cases,
	// the padding-aware decoder works.
	return base64.StdEncoding.DecodeString(data)
}

// CallInfo describes an RPC as it runs. Clients reach it with
// [NewClientContext], servers with [CallInfoForServerContext]. The two sides
// use separate context keys, so a handler's outbound calls don't share its
// inbound metadata.
//
// The transport populates the fields: request-side fields before dispatch,
// response-side fields and stats as messages flow. On a streaming RPC,
// reading a field before the matching Send or Receive returns races the
// transport.
type CallInfo struct {
	// Spec describes the RPC procedure.
	Spec Spec
	// PeerAddr is the remote peer's address, or empty when the transport has
	// no network peer.
	PeerAddr string
	// Protocol is the wire protocol, such as "connect", "grpc", or "grpcweb".
	Protocol string
	// Codec is the codec name, such as "proto" or "json".
	Codec string
	// RequestEncoding is the request compression, "identity" when
	// uncompressed.
	RequestEncoding string
	// ResponseEncoding is the response compression, "identity" when
	// uncompressed.
	ResponseEncoding string
	// SendStats holds byte counts for the most recent Send.
	SendStats MessageStats
	// ReceiveStats holds byte counts for the most recent Receive.
	ReceiveStats MessageStats
	// TransportInfo is optional transport-specific metadata, such as
	// [connectrpc.com/connect/v2/connecthttp.ClientInfo] or
	// [connectrpc.com/connect/v2/connecthttp.ServerInfo].
	TransportInfo any

	request, response, trailer Header
}

// RequestHeader returns the request headers. The client sets them before the
// first Send, and the server reads them.
func (c *CallInfo) RequestHeader() *Header { return &c.request }

// ResponseHeader returns the response headers. The server sets them before
// its first Send, and the client reads them once the response arrives.
func (c *CallInfo) ResponseHeader() *Header { return &c.response }

// ResponseTrailer returns the response trailers. The server sets them before
// the stream ends, and the client reads them after receiving to [io.EOF].
func (c *CallInfo) ResponseTrailer() *Header { return &c.trailer }

// MessageStats holds byte counts for one RPC message. The counts exclude
// envelope framing, HTTP headers, and trailers.
type MessageStats struct {
	// Size is the uncompressed payload size in bytes.
	Size int
	// CompressedSize is the compressed payload size in bytes. It is zero when
	// the message is uncompressed.
	CompressedSize int
}

// Codec marshals and unmarshals RPC messages. Implementations must be safe
// for concurrent use. Method contexts carry per-call values. Codecs are not
// required to observe cancellation.
//
// Transports pass [io.Writer] and [io.Reader] so codecs may stream when their
// encoding supports it. Codecs that need contiguous input or output should
// buffer internally. Writers from this module also expose the AvailableBuffer
// and Grow methods of [bytes.Buffer] for append-style encoders.
//
// dst and src are valid only for the duration of the call. A codec must
// not retain or use either after it returns.
type Codec interface {
	// Name returns the protocol codec token, such as "proto" or "json".
	Name() string
	// MarshalWrite encodes msg to dst. The encoding must be complete when
	// MarshalWrite returns. Any write error from dst must be returned
	// directly or wrapped with %w.
	MarshalWrite(ctx context.Context, dst io.Writer, msg any) error
	// UnmarshalRead decodes one message from src into msg. src is bounded to
	// that message and reports [io.EOF] once the payload is consumed. Any read
	// error from src must be returned directly or wrapped with %w. The transport
	// drains any unread bytes once it returns.
	UnmarshalRead(ctx context.Context, src io.Reader, msg any) error
}

// StableCodec is a [Codec] with a deterministic encoding. Transports use
// stable encodings for features such as Connect GET request URLs.
type StableCodec interface {
	Codec
	// MarshalWriteStable encodes msg deterministically to dst. Messages
	// equivalent under the codec's schema rules must produce identical bytes.
	// The dst writer follows the rules of [Codec.MarshalWrite].
	MarshalWriteStable(ctx context.Context, dst io.Writer, msg any) error
	// IsBinary reports whether MarshalWriteStable writes binary data. Binary
	// encodings are text-encoded before use in URLs.
	IsBinary() bool
}

// Compressor compresses and decompresses RPC payloads. Implementations must
// be safe for concurrent use.
//
// The writers and readers returned by Compressor methods own per-payload state.
// They are used by one goroutine at a time and must be closed exactly once.
// Implementations may pool state and recycle it on Close.
//
// The returned writer may retain dst until Close, and the returned reader may
// retain src until Close. They must not write to dst or read from src after
// Close. Transports enforce decompressed-size limits by wrapping the reader
// returned from Decompress.
type Compressor interface {
	// Name returns the content-encoding token, such as "gzip".
	Name() string
	// Compress returns a writer that writes compressed bytes to dst. Closing
	// the writer flushes buffered data and releases resources. Callers must
	// close the writer exactly once and must not use it after Close.
	Compress(dst io.Writer) (io.WriteCloser, error)
	// Decompress returns a reader for the decompressed form of src. Closing
	// the reader releases resources without closing src. Callers must close
	// the reader exactly once, even if it was not fully consumed, and must not
	// use it after Close.
	Decompress(src io.Reader) (io.ReadCloser, error)
}

// Code is a Connect error code.
type Code uint32

// String returns the lowercase protocol token for c.
func (c Code) String() string {
	switch c {
	case CodeCanceled:
		return "canceled"
	case CodeUnknown:
		return "unknown"
	case CodeInvalidArgument:
		return "invalid_argument"
	case CodeDeadlineExceeded:
		return "deadline_exceeded"
	case CodeNotFound:
		return "not_found"
	case CodeAlreadyExists:
		return "already_exists"
	case CodePermissionDenied:
		return "permission_denied"
	case CodeResourceExhausted:
		return "resource_exhausted"
	case CodeFailedPrecondition:
		return "failed_precondition"
	case CodeAborted:
		return "aborted"
	case CodeOutOfRange:
		return "out_of_range"
	case CodeUnimplemented:
		return "unimplemented"
	case CodeInternal:
		return "internal"
	case CodeUnavailable:
		return "unavailable"
	case CodeDataLoss:
		return "data_loss"
	case CodeUnauthenticated:
		return "unauthenticated"
	}
	return fmt.Sprintf("code_%d", c)
}

// MarshalText implements [encoding.TextMarshaler].
func (c Code) MarshalText() ([]byte, error) {
	return []byte(c.String()), nil
}

// UnmarshalText implements [encoding.TextUnmarshaler].
func (c *Code) UnmarshalText(data []byte) error {
	dataStr := string(data)
	switch dataStr {
	case "canceled":
		*c = CodeCanceled
		return nil
	case "unknown":
		*c = CodeUnknown
		return nil
	case "invalid_argument":
		*c = CodeInvalidArgument
		return nil
	case "deadline_exceeded":
		*c = CodeDeadlineExceeded
		return nil
	case "not_found":
		*c = CodeNotFound
		return nil
	case "already_exists":
		*c = CodeAlreadyExists
		return nil
	case "permission_denied":
		*c = CodePermissionDenied
		return nil
	case "resource_exhausted":
		*c = CodeResourceExhausted
		return nil
	case "failed_precondition":
		*c = CodeFailedPrecondition
		return nil
	case "aborted":
		*c = CodeAborted
		return nil
	case "out_of_range":
		*c = CodeOutOfRange
		return nil
	case "unimplemented":
		*c = CodeUnimplemented
		return nil
	case "internal":
		*c = CodeInternal
		return nil
	case "unavailable":
		*c = CodeUnavailable
		return nil
	case "data_loss":
		*c = CodeDataLoss
		return nil
	case "unauthenticated":
		*c = CodeUnauthenticated
		return nil
	}
	// Ensure that non-canonical codes round-trip through MarshalText and
	// UnmarshalText.
	if after, ok := strings.CutPrefix(dataStr, "code_"); ok {
		dataStr = after
		code, err := strconv.ParseUint(dataStr, 10 /* base */, 32 /* bitsize */)
		if err == nil && (code < uint64(minCode) || code > uint64(maxCode)) {
			*c = Code(code)
			return nil
		}
	}
	return fmt.Errorf("invalid code %q", dataStr)
}

// Error is the Connect error type. Code, message, and supported detail
// values may be serialized by transports. Cause and remote are local process
// metadata and are never serialized.
//
// Handlers fail an RPC by returning an error, but only a locally authored
// *Error carries its code, message, and details to the wire. Transports
// must not serialize other errors: clients see only a code, typically
// [CodeUnknown], with no message. Text sent to callers is therefore always
// constructed intentionally with [NewError] or [Errorf].
type Error struct {
	code    Code
	message string
	details []*ErrorDetail
	cause   error
	remote  bool
}

// ErrorDetail is a self-describing message attached to an [*Error]. On the
// Connect, gRPC, and gRPC-Web protocols, details are Protobuf messages:
// construct them with
// [connectrpc.com/connect/v2/connectproto.NewErrorDetail] and decode them
// with [connectrpc.com/connect/v2/connectproto.UnmarshalErrorDetail].
type ErrorDetail struct {
	// Type is the fully-qualified message type name, such as
	// "google.rpc.RetryInfo".
	Type string
	// Value is the serialized message.
	Value []byte
	// Debug is an optional human-readable representation of Value, carried
	// by the Connect protocol as the detail's "debug" JSON. It is best
	// effort: transports may regenerate or omit it.
	Debug []byte
}

// NewError returns a new [*Error] with code and a public message.
// The message is serialized to the wire. Do not include sensitive details.
func NewError(code Code, message string) *Error {
	return &Error{code: code, message: message}
}

// Errorf returns a new [*Error] with the given code and a formatted public
// message. The message is serialized to the wire. Do not include sensitive
// details.
//
// Errorf uses fmt.Sprintf and does not attach wrapped errors. Use [Error.WithCause]
// for local-only causes.
func Errorf(code Code, format string, args ...any) *Error {
	return NewError(code, fmt.Sprintf(format, args...))
}

// Code returns the Connect error code. If the stored code is zero, Code returns
// [CodeUnknown].
func (e *Error) Code() Code {
	if e == nil || e.code == 0 {
		return CodeUnknown
	}
	return e.code
}

// Message returns the optional human-readable error message serialized on the
// wire.
func (e *Error) Message() string {
	if e == nil {
		return ""
	}
	return e.message
}

// Details returns a copy of the error's detail list. Decode each detail with
// a codec package, such as
// [connectrpc.com/connect/v2/connectproto.UnmarshalErrorDetail].
func (e *Error) Details() []*ErrorDetail {
	return slices.Clone(e.details)
}

// IsRemote reports whether this error is a peer's RPC verdict rather than
// the local handler's own. Server transports must not forward a remote Error
// as the handler's own RPC verdict.
func (e *Error) IsRemote() bool {
	return e != nil && e.remote
}

// Unwrap returns the local cause for errors.Is and errors.As. The cause is not
// serialized.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Error returns "code" or "code: message".
func (e *Error) Error() string {
	code := e.Code()
	if e.Message() == "" {
		return code.String()
	}
	return code.String() + ": " + e.Message()
}

// WithDetail returns a cloned error with detail appended as a public detail
// value. Construct details with a codec package, such as
// [connectrpc.com/connect/v2/connectproto.NewErrorDetail]. A nil detail is
// ignored.
func (e *Error) WithDetail(detail *ErrorDetail) *Error {
	if detail == nil {
		return e
	}
	clone := *e
	clone.details = make([]*ErrorDetail, len(e.details), len(e.details)+1)
	copy(clone.details, e.details)
	clone.details = append(clone.details, detail)
	return &clone
}

// WithCause returns a cloned error with err attached as a local cause. Causes
// are available to errors.Is and errors.As but are not serialized. A nil cause
// is ignored.
func (e *Error) WithCause(err error) *Error {
	if err == nil {
		return e
	}
	clone := *e
	if clone.cause == nil {
		clone.cause = err
	} else {
		clone.cause = errors.Join(clone.cause, err)
	}
	return &clone
}

// WithRemote returns a cloned error marked as a peer's RPC verdict.
func (e *Error) WithRemote() *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.remote = true
	return &clone
}

// NewClientContext attaches a fresh client-side [CallInfo] to ctx and returns
// both. Use it to set request metadata before issuing an RPC, or to read
// response metadata after the call. A reused handle reports the most recent
// call. For concurrent calls, derive a separate context per call.
func NewClientContext(ctx context.Context) (context.Context, *CallInfo) {
	info := &CallInfo{}
	return context.WithValue(ctx, clientInfoKey{}, info), info
}

// CallInfoForClientContext returns the client-side [CallInfo] attached to
// ctx, if there is one. [Client] call methods attach a fresh CallInfo before
// invoking the interceptor chain when ctx does not already carry one, so
// client interceptors and the [Transport] can rely on it being present.
// Outside a Client call, it reports false unless the caller used
// [NewClientContext].
func CallInfoForClientContext(ctx context.Context) (*CallInfo, bool) {
	info, _ := ctx.Value(clientInfoKey{}).(*CallInfo)
	return info, info != nil
}

// CallInfoForServerContext returns the server-side [CallInfo] attached by
// [Server.Call] before invoking interceptors and generated handlers, if there
// is one. It reports false if the call did not come through [Server.Call]
// (e.g., a raw [ServerFunc] invocation in a test).
func CallInfoForServerContext(ctx context.Context) (*CallInfo, bool) {
	info, _ := ctx.Value(serverInfoKey{}).(*CallInfo)
	return info, info != nil
}

// CodeOf returns the Connect code carried by err. If err does not wrap an
// [*Error], CodeOf returns [CodeUnknown]. Passing nil is not meaningful and
// also returns CodeUnknown.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code()
	}
	return CodeUnknown
}

type (
	clientInfoKey struct{}
	serverInfoKey struct{}
)

func chainClientInterceptors(ics []ClientInterceptor, next ClientFunc) ClientFunc {
	for _, interceptor := range slices.Backward(ics) {
		next = interceptor(next)
	}
	return next
}

func chainServerInterceptors(ics []ServerInterceptor) ServerInterceptor {
	switch len(ics) {
	case 0:
		return nil
	case 1:
		return ics[0]
	}
	chain := make([]ServerInterceptor, len(ics))
	copy(chain, ics)
	return func(next ServerFunc) ServerFunc {
		for _, interceptor := range slices.Backward(chain) {
			next = interceptor(next)
		}
		return next
	}
}

func withServerContext(ctx context.Context, info *CallInfo) context.Context {
	return context.WithValue(ctx, serverInfoKey{}, info)
}

// ensureClientContext returns a ctx carrying a client-side CallInfo. A reused
// CallInfo has transport fields reset.
func ensureClientContext(ctx context.Context) context.Context {
	if info, ok := CallInfoForClientContext(ctx); ok {
		info.Spec = Spec{}
		info.PeerAddr = ""
		info.Protocol = ""
		info.Codec = ""
		info.RequestEncoding = ""
		info.ResponseEncoding = ""
		info.TransportInfo = nil
		info.SendStats = MessageStats{}
		info.ReceiveStats = MessageStats{}
		clear(info.response.store)
		clear(info.trailer.store)
		return ctx
	}
	ctx, _ = NewClientContext(ctx)
	return ctx
}

// clearClientContext shadows any inbound client-side CallInfo with a nil
// value so a server handler's context never exposes the caller's client
// CallInfo. A handler is server-side: it reads its own CallInfo through
// [CallInfoForServerContext], and a nested outbound RPC must start with a fresh
// client CallInfo. Without this, a transport that dispatches the handler on
// the caller's context (such as the in-process transport) would let that
// nested RPC reuse the caller's request headers and overwrite the caller's
// response metadata. HTTP transports build the handler context from the
// request, which never carries a client CallInfo, so this is a no-op for
// them.
func clearClientContext(ctx context.Context) context.Context {
	if _, ok := CallInfoForClientContext(ctx); !ok {
		return ctx
	}
	return context.WithValue(ctx, clientInfoKey{}, (*CallInfo)(nil))
}
