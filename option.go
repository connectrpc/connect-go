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

package connect

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"slices"
)

// A ClientOption configures a [Client].
//
// In addition to any options grouped in the documentation below, remember that
// any [Option] is also a valid ClientOption.
type ClientOption interface {
	applyToClient(*clientConfig)
}

// A HandlerOption configures a [Handler].
//
// In addition to any options grouped in the documentation below, remember that
// any [Option] is also a HandlerOption.
type HandlerOption interface {
	applyToHandler(*handlerConfig)
}

// Option implements both [ClientOption] and [HandlerOption], so it can be
// applied both client-side and server-side.
type Option interface {
	ClientOption
	HandlerOption
}

// clientConfig is the v1 client configuration accumulated from options and
// then projected into [connecthttp] and v1 runtime state when the [Client] is
// built.
type clientConfig struct {
	procedure        string
	schema           any
	idempotencyLevel IdempotencyLevel

	interceptors []Interceptor
	initializer  maybeInitializer

	protocol            string // "" connect, "grpc", "grpcweb"
	enableGet           bool
	getURLMaxBytes      int
	getUseFallback      bool
	sendCodec           Codec  // user-supplied via WithCodec
	sendCompressionName string // user-supplied via WithSendCompression
	codecOverrides      []Codec
	compressors         map[string]*compressorEntry // by name, in order
	compressorOrder     []string
	compressMinBytes    int
	readMaxBytes        int
	sendMaxBytes        int
}

// handlerConfig mirrors clientConfig for the server side. The shim builds a
// connecthttp-mounted v2 server from this state when [Handler] is created.
type handlerConfig struct {
	procedure        string
	streamType       StreamType
	schema           any
	idempotencyLevel IdempotencyLevel

	interceptors []Interceptor
	initializer  maybeInitializer

	codecOverrides         []Codec
	compressors            map[string]*compressorEntry
	compressorOrder        []string
	compressMinBytes       int
	readMaxBytes           int
	sendMaxBytes           int
	requireConnectProtocol bool
	requestGates           []RequestGateFunc
}

// compressorEntry holds the factory pair for a named v1 compressor.
type compressorEntry struct {
	newDecompressor func() Decompressor
	newCompressor   func() Compressor
}

// newClientConfig produces a clientConfig with the v1 defaults applied (proto
// codec, gzip accept) plus the user's options.
func newClientConfig(procedure string, options []ClientOption) *clientConfig {
	cfg := &clientConfig{
		procedure:   procedure,
		compressors: make(map[string]*compressorEntry),
	}
	cfg.applyDefaultsClient()
	for _, opt := range options {
		opt.applyToClient(cfg)
	}
	return cfg
}

// newHandlerConfig produces a handlerConfig with v1 defaults applied (proto,
// json codecs, gzip support) plus the user's options.
func newHandlerConfig(procedure string, streamType StreamType, options []HandlerOption) *handlerConfig {
	cfg := &handlerConfig{
		procedure:   procedure,
		streamType:  streamType,
		compressors: make(map[string]*compressorEntry),
	}
	cfg.applyDefaultsHandler()
	for _, opt := range options {
		opt.applyToHandler(cfg)
	}
	return cfg
}

// applyDefaultsClient registers gzip in the accept list (matching v1 default
// behavior). The proto codec is the v2 default codec.
func (c *clientConfig) applyDefaultsClient() {
	c.compressors[compressionGzip] = newGzipEntry()
	c.compressorOrder = append(c.compressorOrder, compressionGzip)
}

// applyDefaultsHandler registers gzip and the default json/proto codecs.
func (c *handlerConfig) applyDefaultsHandler() {
	c.compressors[compressionGzip] = newGzipEntry()
	c.compressorOrder = append(c.compressorOrder, compressionGzip)
}

// validate reports configuration errors that v1 surfaces when the client is
// constructed rather than on each call.
func (c *clientConfig) validate() *Error {
	if c.sendCompressionName != "" && c.sendCompressionName != compressionIdentity {
		if _, ok := c.compressors[c.sendCompressionName]; !ok {
			return errorf(CodeUnknown, "unknown compression %q", c.sendCompressionName)
		}
	}
	return nil
}

// newSpec returns the v1 [Spec] described by this client config. IsClient is
// true.
func (c *clientConfig) newSpec(t StreamType) Spec {
	return Spec{
		StreamType:       t,
		Procedure:        c.procedure,
		Schema:           c.schema,
		IsClient:         true,
		IdempotencyLevel: c.idempotencyLevel,
	}
}

// newSpec returns the v1 [Spec] described by this handler config. IsClient is
// false.
func (c *handlerConfig) newSpec() Spec {
	return Spec{
		StreamType:       c.streamType,
		Procedure:        c.procedure,
		Schema:           c.schema,
		IsClient:         false,
		IdempotencyLevel: c.idempotencyLevel,
	}
}

// chainInterceptor returns the composed interceptor or nil if none are
// registered. The first interceptor in c.interceptors is the outermost
// wrapper.
func (c *clientConfig) chainInterceptor() Interceptor {
	switch len(c.interceptors) {
	case 0:
		return nil
	case 1:
		return c.interceptors[0]
	}
	return newChain(c.interceptors)
}

// chainInterceptor returns the composed interceptor or nil if none are
// registered.
func (c *handlerConfig) chainInterceptor() Interceptor {
	switch len(c.interceptors) {
	case 0:
		return nil
	case 1:
		return c.interceptors[0]
	}
	return newChain(c.interceptors)
}

// newGzipEntry returns the default gzip compressor pair, matching v1's
// pre-shim behavior.
func newGzipEntry() *compressorEntry {
	return &compressorEntry{
		newDecompressor: func() Decompressor { return &gzip.Reader{} },
		newCompressor:   func() Compressor { return gzip.NewWriter(io.Discard) },
	}
}

// WithAcceptCompression makes a compression algorithm available to a client.
// Clients ask servers to compress responses using any of the registered
// algorithms. The first registered algorithm is treated as the least
// preferred, and the last registered algorithm is the most preferred.
//
// It's safe to use this option liberally: servers will ignore any
// compression algorithms they don't support. To compress requests, pair this
// option with [WithSendCompression]. To remove support for a
// previously-registered compression algorithm, use WithAcceptCompression with
// nil decompressor and compressor constructors.
//
// Clients accept gzipped responses by default, using a compressor backed by the
// standard library's [gzip] package with the default compression level. Use
// [WithSendGzip] to compress requests with gzip.
//
// Calling WithAcceptCompression with an empty name is a no-op.
func WithAcceptCompression(
	name string,
	newDecompressor func() Decompressor,
	newCompressor func() Compressor,
) ClientOption {
	return &compressionOption{
		name:            name,
		newDecompressor: newDecompressor,
		newCompressor:   newCompressor,
	}
}

// WithCompression configures handlers to support a compression algorithm.
// Clients may send messages compressed with that algorithm and/or request
// compressed responses. The [Compressor] and [Decompressor] produced by the
// supplied constructors must use the same algorithm. Internally, Connect pools
// compressors and decompressors.
//
// By default, handlers support gzip using the standard library's
// [compress/gzip] package at the default compression level. To remove support for
// a previously-registered compression algorithm, use WithCompression with nil
// decompressor and compressor constructors.
//
// Calling WithCompression with an empty name is a no-op.
func WithCompression(
	name string,
	newDecompressor func() Decompressor,
	newCompressor func() Compressor,
) HandlerOption {
	return &compressionOption{
		name:            name,
		newDecompressor: newDecompressor,
		newCompressor:   newCompressor,
	}
}

// WithClientOptions composes multiple ClientOptions into one.
func WithClientOptions(options ...ClientOption) ClientOption {
	return &clientOptionsBundle{options: options}
}

// WithHandlerOptions composes multiple HandlerOptions into one.
func WithHandlerOptions(options ...HandlerOption) HandlerOption {
	return &handlerOptionsBundle{options: options}
}

// WithOptions composes multiple Options into one.
func WithOptions(options ...Option) Option {
	return &optionsBundle{options: options}
}

// WithGRPC configures clients to use the HTTP/2 gRPC protocol.
func WithGRPC() ClientOption { return &protocolClientOption{name: "grpc"} }

// WithGRPCWeb configures clients to use the gRPC-Web protocol.
func WithGRPCWeb() ClientOption { return &protocolClientOption{name: "grpcweb"} }

// WithProtoJSON configures a client to send JSON-encoded data instead of
// binary Protobuf. It uses the standard Protobuf JSON mapping as implemented
// by [google.golang.org/protobuf/encoding/protojson]: fields are named using
// lowerCamelCase, zero values are omitted, missing required fields are errors,
// enums are emitted as strings, etc.
func WithProtoJSON() ClientOption {
	return WithCodec(&protoJSONCodec{name: codecNameJSON})
}

// WithSendCompression configures the client to use the specified algorithm to
// compress request messages. If the algorithm has not been registered using
// [WithAcceptCompression], the client will return errors at runtime.
//
// Because some servers don't support compression, clients default to sending
// uncompressed requests.
func WithSendCompression(name string) ClientOption {
	return &sendCompressionOption{name: name}
}

// WithSendGzip configures the client to gzip requests. Since clients have
// access to a gzip compressor by default, WithSendGzip doesn't require
// [WithSendCompression].
//
// Some servers don't support gzip, so clients default to sending uncompressed
// requests.
func WithSendGzip() ClientOption { return WithSendCompression(compressionGzip) }

// WithRecover adds an interceptor that recovers from panics. The supplied
// function receives the context, [Spec], request headers, and the recovered
// value (which may be nil). It must return an error to send back to the
// client. It may also log the panic, emit metrics, or execute other
// error-handling logic. Handler functions must be safe to call concurrently.
//
// To preserve compatibility with [net/http]'s semantics, this interceptor
// doesn't handle panics with [http.ErrAbortHandler].
//
// By default, handlers don't recover from panics. Because the standard
// library's [http.Server] recovers from panics by default, this option isn't
// usually necessary to prevent crashes. Instead, it helps servers collect
// RPC-specific data during panics and send a more detailed error to
// clients.
func WithRecover(handle func(context.Context, Spec, http.Header, any) error) HandlerOption {
	return WithInterceptors(&recoverHandlerInterceptor{handle: handle})
}

// A RequestGateFunc runs before an RPC reaches the interceptor chain and the
// handler. See [WithRequestGate].
type RequestGateFunc func(ctx context.Context, spec Spec, peer Peer, header http.Header) (context.Context, error)

// WithRequestGate registers a function that runs once the request headers are
// available, but before any message is received or decoded.
//
// If the gate returns an error, the RPC is rejected immediately. The error is
// sent to the client and neither the interceptor chain nor the handler runs.
// A gate may return a non-nil context to update the context. It must be derived
// from the passed context.
//
// Gates are ideal for authentication. Rejecting an unauthenticated call inside
// an [Interceptor] is inefficient because the request message has already been
// decoded. By using a gate, you avoid this cost. Because rejected RPCs never
// reach the interceptor chain, logging or metrics interceptors will not observe
// them. For even earlier rejection, use standard net/http middleware.
//
// Multiple WithRequestGate options accumulate and run in the order they were
// applied. Gates must be safe to call concurrently. Panics inside a gate are
// not recovered by [WithRecover], which only wraps interceptors and handlers.
//
// WithRequestGate panics if the provided gate is nil.
func WithRequestGate(gate RequestGateFunc) HandlerOption {
	if gate == nil {
		panic("connect: WithRequestGate called with nil gate") //nolint:forbidigo
	}
	return &requestGateOption{gate: gate}
}

// WithRequireConnectProtocolHeader configures the Handler to require requests
// using the Connect RPC protocol to include the Connect-Protocol-Version
// header. This ensures that HTTP proxies and net/http middleware can easily
// identify valid Connect requests, even if they use a common Content-Type like
// application/json. However, it makes ad-hoc requests with tools like cURL
// more laborious. Streaming requests are not affected by this option.
//
// This option has no effect if the client uses the gRPC or gRPC-Web protocols.
func WithRequireConnectProtocolHeader() HandlerOption {
	return &requireConnectProtocolHeaderOption{}
}

// WithConditionalHandlerOptions allows procedures in the same service to have
// different configurations: for example, one procedure may need a much larger
// WithReadMaxBytes setting than the others.
//
// WithConditionalHandlerOptions takes a function which may inspect each
// procedure's Spec before deciding which options to apply. Returning a nil
// slice is safe.
func WithConditionalHandlerOptions(conditional func(spec Spec) []HandlerOption) HandlerOption {
	return &conditionalHandlerOptions{conditional: conditional}
}

// WithSchema provides a parsed representation of the schema for an RPC to a
// client or handler. The supplied schema is exposed as [Spec].Schema. This
// option is typically added by generated code.
//
// For services using protobuf schemas, the supplied schema should be a
// [google.golang.org/protobuf/reflect/protoreflect.MethodDescriptor].
func WithSchema(schema any) Option { return &schemaOption{schema: schema} }

// WithRequestInitializer provides a function that initializes a new message.
// It may be used to dynamically construct request messages. It is called on
// server receives to construct the message to be unmarshaled into. The message
// will be a non nil pointer to the type created by the handler. Use the Schema
// field of the [Spec] to determine the type of the message.
func WithRequestInitializer(initializer func(spec Spec, message any) error) HandlerOption {
	return &initializerHandlerOption{initializer: initializer}
}

// WithResponseInitializer provides a function that initializes a new message.
// It may be used to dynamically construct response messages. It is called on
// client receives to construct the message to be unmarshaled into. The message
// will be a non nil pointer to the type created by the client. Use the Schema
// field of the [Spec] to determine the type of the message.
func WithResponseInitializer(initializer func(spec Spec, message any) error) ClientOption {
	return &initializerClientOption{initializer: initializer}
}

// WithCodec registers a serialization method with a client or handler.
// Handlers may have multiple codecs registered, and use whichever the client
// chooses. Clients may only have a single codec.
//
// By default, handlers and clients support binary Protocol Buffer data using
// [google.golang.org/protobuf/proto]. Handlers also support JSON by default,
// using the standard Protobuf JSON mapping. Users with more specialized needs
// may override the default codecs by registering a new codec under the "proto"
// or "json" names. When supplying a custom "proto" codec, keep in mind that
// some unexported, protocol-specific messages are serialized using Protobuf -
// take care to fall back to the standard Protobuf implementation if
// necessary.
//
// Registering a codec with an empty name is a no-op.
func WithCodec(codec Codec) Option { return &codecOption{codec: codec} }

// WithCompressMinBytes sets a minimum size threshold for compression:
// regardless of compressor configuration, messages smaller than the configured
// minimum are sent uncompressed.
//
// The default minimum is zero. Setting a minimum compression threshold may
// improve overall performance, because the CPU cost of compressing very small
// messages usually isn't worth the small reduction in network I/O.
func WithCompressMinBytes(minBytes int) Option {
	return &compressMinBytesOption{minBytes: minBytes}
}

// WithReadMaxBytes limits the performance impact of pathologically large
// messages sent by the other party. For handlers, WithReadMaxBytes limits the size
// of a message that the client can send. For clients, WithReadMaxBytes limits the
// size of a message that the server can respond with. Limits apply to each Protobuf
// message, not to the stream as a whole.
//
// Setting WithReadMaxBytes to zero allows any message size. Both clients and
// handlers default to allowing any request size.
//
// Handlers may also use [http.MaxBytesHandler] to limit the total size of the
// HTTP request stream (rather than the per-message size). Connect handles
// [http.MaxBytesError] specially, so clients still receive errors with the
// appropriate error code and informative messages.
func WithReadMaxBytes(maxBytes int) Option {
	return &readMaxBytesOption{maxBytes: maxBytes}
}

// WithSendMaxBytes prevents sending messages too large for the client/handler
// to handle without significant performance overhead. For handlers, WithSendMaxBytes
// limits the size of a message that the handler can respond with. For clients,
// WithSendMaxBytes limits the size of a message that the client can send. Limits
// apply to each message, not to the stream as a whole.
//
// Handler errors are always sent. If an error exceeds the limit, the handler
// sends a minimal error with only the code and message.
//
// Setting WithSendMaxBytes to zero allows any message size. Both clients and
// handlers default to allowing any message size.
func WithSendMaxBytes(maxBytes int) Option {
	return &sendMaxBytesOption{maxBytes: maxBytes}
}

// WithIdempotency declares the idempotency of the procedure. This can determine
// whether a procedure call can safely be retried, and may affect which request
// modalities are allowed for a given procedure call.
//
// In most cases, you should not need to manually set this. It is normally set
// by the code generator for your schema. For protobuf schemas, it can be set like this:
//
//	rpc Ping(PingRequest) returns (PingResponse) {
//	  option idempotency_level = NO_SIDE_EFFECTS;
//	}
func WithIdempotency(idempotencyLevel IdempotencyLevel) Option {
	return &idempotencyOption{idempotencyLevel: idempotencyLevel}
}

// WithHTTPGet allows Connect-protocol clients to use HTTP GET requests for
// side-effect free unary RPC calls. Typically, the service schema indicates
// which procedures are idempotent (see [WithIdempotency] for an example
// protobuf schema). The gRPC and gRPC-Web protocols are POST-only, so this
// option has no effect when combined with [WithGRPC] or [WithGRPCWeb].
//
// Using HTTP GET requests makes it easier to take advantage of CDNs, caching
// reverse proxies, and browsers' built-in caching. Note, however, that servers
// don't automatically set any cache headers; you can set cache headers using
// interceptors or by adding headers in individual procedure implementations.
//
// By default, all requests are made as HTTP POSTs.
func WithHTTPGet() ClientOption { return &enableGetOption{} }

// WithHTTPGetMaxURLSize sets the maximum allowable URL length for GET requests
// made using the Connect protocol. It has no effect on gRPC or gRPC-Web
// clients, since those protocols are POST-only.
//
// Limiting the URL size is useful as most user agents, proxies, and servers
// have limits on the allowable length of a URL. For example, Apache and Nginx
// limit the size of a request line to around 8 KiB, meaning that maximum
// length of a URL is a bit smaller than this. If you run into URL size
// limitations imposed by your network infrastructure and don't know the
// maximum allowable size, or if you'd prefer to be cautious from the start, a
// 4096 byte (4 KiB) limit works with most common proxies and CDNs.
//
// If fallback is set to true and the URL would be longer than the configured
// maximum value, the request will be sent as an HTTP POST instead. If fallback
// is set to false, the request will fail with [CodeResourceExhausted].
//
// By default, Connect-protocol clients with GET requests enabled may send a
// URL of any size.
func WithHTTPGetMaxURLSize(bytes int, fallback bool) ClientOption {
	return &getURLMaxBytesOption{maxBytes: bytes, fallback: fallback}
}

// WithInterceptors configures a client or handler's interceptor stack. Repeated
// WithInterceptors options are applied in order, so
//
//	WithInterceptors(A) + WithInterceptors(B, C) == WithInterceptors(A, B, C)
//
// Unary interceptors compose like an onion. The first interceptor provided is
// the outermost layer of the onion: it acts first on the context and request,
// and last on the response and error.
//
// Stream interceptors also behave like an onion: the first interceptor
// provided is the outermost wrapper for the [StreamingClientConn] or
// [StreamingHandlerConn]. It's the first to see sent messages and the last to
// see received messages.
//
// Applied to client and handler, WithInterceptors(A, B, ..., Y, Z) produces:
//
//	 client.Send()       client.Receive()
//	       |                   ^
//	       v                   |
//	    A ---                 --- A
//	    B ---                 --- B
//	    : ...                 ... :
//	    Y ---                 --- Y
//	    Z ---                 --- Z
//	       |                   ^
//	       v                   |
//	  = = = = = = = = = = = = = = = =
//	               network
//	  = = = = = = = = = = = = = = = =
//	       |                   ^
//	       v                   |
//	    A ---                 --- A
//	    B ---                 --- B
//	    : ...                 ... :
//	    Y ---                 --- Y
//	    Z ---                 --- Z
//	       |                   ^
//	       v                   |
//	handler.Receive()   handler.Send()
//	       |                   ^
//	       |                   |
//	       '-> handler logic >-'
//
// Note that in clients, Send handles the request message(s) and Receive
// handles the response message(s). For handlers, it's the reverse. Depending
// on your interceptor's logic, you may need to wrap one method in clients and
// the other in handlers.
func WithInterceptors(interceptors ...Interceptor) Option {
	return &interceptorsOption{interceptors: interceptors}
}

// ---------------------------------------------------------------------------
// option impls
// ---------------------------------------------------------------------------

type schemaOption struct{ schema any }

func (o *schemaOption) applyToClient(c *clientConfig)   { c.schema = o.schema }
func (o *schemaOption) applyToHandler(c *handlerConfig) { c.schema = o.schema }

type idempotencyOption struct{ idempotencyLevel IdempotencyLevel }

func (o *idempotencyOption) applyToClient(c *clientConfig)   { c.idempotencyLevel = o.idempotencyLevel }
func (o *idempotencyOption) applyToHandler(c *handlerConfig) { c.idempotencyLevel = o.idempotencyLevel }

type codecOption struct{ codec Codec }

func (o *codecOption) applyToClient(c *clientConfig) {
	if o.codec == nil || o.codec.Name() == "" {
		return
	}
	c.sendCodec = o.codec
	c.codecOverrides = append(c.codecOverrides, o.codec)
}

func (o *codecOption) applyToHandler(c *handlerConfig) {
	if o.codec == nil || o.codec.Name() == "" {
		return
	}
	c.codecOverrides = append(c.codecOverrides, o.codec)
}

type compressionOption struct {
	name            string
	newDecompressor func() Decompressor
	newCompressor   func() Compressor
}

func (o *compressionOption) applyToClient(c *clientConfig) {
	o.applyTo(&c.compressorOrder, c.compressors)
}

func (o *compressionOption) applyToHandler(c *handlerConfig) {
	o.applyTo(&c.compressorOrder, c.compressors)
}

func (o *compressionOption) applyTo(order *[]string, pools map[string]*compressorEntry) {
	if o.name == "" {
		return
	}
	if o.newDecompressor == nil && o.newCompressor == nil {
		delete(pools, o.name)
		filtered := (*order)[:0]
		for _, n := range *order {
			if n != o.name {
				filtered = append(filtered, n)
			}
		}
		*order = filtered
		return
	}
	// Re-registering a name moves it to the end, making it the most preferred.
	*order = slices.DeleteFunc(*order, func(n string) bool { return n == o.name })
	*order = append(*order, o.name)
	pools[o.name] = &compressorEntry{
		newDecompressor: o.newDecompressor,
		newCompressor:   o.newCompressor,
	}
}

type compressMinBytesOption struct{ minBytes int }

func (o *compressMinBytesOption) applyToClient(c *clientConfig)   { c.compressMinBytes = o.minBytes }
func (o *compressMinBytesOption) applyToHandler(c *handlerConfig) { c.compressMinBytes = o.minBytes }

type readMaxBytesOption struct{ maxBytes int }

func (o *readMaxBytesOption) applyToClient(c *clientConfig)   { c.readMaxBytes = o.maxBytes }
func (o *readMaxBytesOption) applyToHandler(c *handlerConfig) { c.readMaxBytes = o.maxBytes }

type sendMaxBytesOption struct{ maxBytes int }

func (o *sendMaxBytesOption) applyToClient(c *clientConfig)   { c.sendMaxBytes = o.maxBytes }
func (o *sendMaxBytesOption) applyToHandler(c *handlerConfig) { c.sendMaxBytes = o.maxBytes }

type protocolClientOption struct{ name string }

func (o *protocolClientOption) applyToClient(c *clientConfig) { c.protocol = o.name }

type enableGetOption struct{}

func (o *enableGetOption) applyToClient(c *clientConfig) { c.enableGet = true }

type getURLMaxBytesOption struct {
	maxBytes int
	fallback bool
}

func (o *getURLMaxBytesOption) applyToClient(c *clientConfig) {
	c.enableGet = true
	c.getURLMaxBytes = o.maxBytes
	c.getUseFallback = o.fallback
}

type requestGateOption struct {
	gate RequestGateFunc
}

func (o *requestGateOption) applyToHandler(c *handlerConfig) {
	c.requestGates = append(c.requestGates, o.gate)
}

type sendCompressionOption struct{ name string }

func (o *sendCompressionOption) applyToClient(c *clientConfig) {
	c.sendCompressionName = o.name
}

type requireConnectProtocolHeaderOption struct{}

func (o *requireConnectProtocolHeaderOption) applyToHandler(c *handlerConfig) {
	c.requireConnectProtocol = true
}

type interceptorsOption struct{ interceptors []Interceptor }

func (o *interceptorsOption) applyToClient(c *clientConfig) {
	c.interceptors = append(c.interceptors, o.interceptors...)
}

func (o *interceptorsOption) applyToHandler(c *handlerConfig) {
	c.interceptors = append(c.interceptors, o.interceptors...)
}

type initializerClientOption struct {
	initializer func(Spec, any) error
}

func (o *initializerClientOption) applyToClient(c *clientConfig) {
	c.initializer = maybeInitializer{initializer: o.initializer}
}

type initializerHandlerOption struct {
	initializer func(Spec, any) error
}

func (o *initializerHandlerOption) applyToHandler(c *handlerConfig) {
	c.initializer = maybeInitializer{initializer: o.initializer}
}

type maybeInitializer struct {
	initializer func(Spec, any) error
}

func (o maybeInitializer) maybe(spec Spec, message any) error {
	if o.initializer != nil {
		return o.initializer(spec, message)
	}
	return nil
}

type conditionalHandlerOptions struct {
	conditional func(spec Spec) []HandlerOption
}

func (o *conditionalHandlerOptions) applyToHandler(cfg *handlerConfig) {
	if o.conditional == nil {
		return
	}
	spec := cfg.newSpec()
	if spec.Procedure == "" {
		return // matches v1's behavior of ignoring empty specs (e.g. ErrorWriter)
	}
	for _, sub := range o.conditional(spec) {
		sub.applyToHandler(cfg)
	}
}

type clientOptionsBundle struct{ options []ClientOption }

func (o *clientOptionsBundle) applyToClient(c *clientConfig) {
	for _, sub := range o.options {
		sub.applyToClient(c)
	}
}

type handlerOptionsBundle struct{ options []HandlerOption }

func (o *handlerOptionsBundle) applyToHandler(c *handlerConfig) {
	for _, sub := range o.options {
		sub.applyToHandler(c)
	}
}

type optionsBundle struct{ options []Option }

func (o *optionsBundle) applyToClient(c *clientConfig) {
	for _, sub := range o.options {
		sub.applyToClient(c)
	}
}

func (o *optionsBundle) applyToHandler(c *handlerConfig) {
	for _, sub := range o.options {
		sub.applyToHandler(c)
	}
}
