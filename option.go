package rerpc

// Option implements both ClientOption and HandlerOption, so it can be applied
// both client-side and server-side.
type Option interface {
	ClientOption
	HandlerOption
}

type overridePkg struct {
	pkg string
}

// OverrideProtobufPackage replaces the protobuf package name set by the
// generated code. This affects URLs and any Specification retrieved from a
// call or handler context. Using this option is usually a bad idea, but it's
// occasionally necessary to prevent protobuf package collisions. (For example,
// reRPC uses this option to serve the health and reflection APIs without
// generating runtime conflicts with grpc-go.)
//
// OverrideProtobufPackage does not change the data exposed by the reflection
// API. To prevent inconsistencies between the reflection data and the actual
// service URL, using this option disables reflection for the overridden
// service (though other services can still be introspected).
func OverrideProtobufPackage(pkg string) Option {
	return &overridePkg{pkg}
}

func (o *overridePkg) applyToClient(cfg *clientCfg) {
	cfg.Package = o.pkg
}

func (o *overridePkg) applyToHandler(cfg *handlerCfg) {
	cfg.Package = o.pkg
	cfg.DisableRegistration = true
}

type readMaxBytes struct {
	Max int64
}

// ReadMaxBytes limits the performance impact of pathologically large messages
// sent by the other party. For handlers, ReadMaxBytes limits the size of
// message that the client can send. For clients, ReadMaxBytes limits the size
// of message that the server can respond with. Limits are applied before
// decompression and apply to each protobuf message, not to the stream as a
// whole.
//
// Setting ReadMaxBytes to zero allows any message size. Both clients and
// handlers default to allowing any request size.
func ReadMaxBytes(n int64) Option {
	return &readMaxBytes{n}
}

func (o *readMaxBytes) applyToClient(cfg *clientCfg) {
	cfg.MaxResponseBytes = o.Max
}

func (o *readMaxBytes) applyToHandler(cfg *handlerCfg) {
	cfg.MaxRequestBytes = o.Max
}

type gzipOption struct {
	Enable bool
}

// Gzip configures client and server compression strategies.
//
// For handlers, enabling gzip compresses responses where it's likely to
// improve overall performance. By default, handlers use gzip if the client
// supports it, the uncompressed response message is >1 KiB, and the message is
// likely to compress well. Disabling gzip support instructs handlers to always
// send uncompressed responses.
//
// For clients, enabling gzip compresses requests where it's likely to improve
// performance (using the same criteria as handlers). gRPC's compression
// negotiation is complex, but most first-party gRPC servers won't compress
// responses unless the client enables this option. Since not all servers
// support gzip compression, clients default to sending uncompressed requests.
func Gzip(enable bool) Option {
	return &gzipOption{enable}
}

func (o *gzipOption) applyToClient(cfg *clientCfg) {
	// NB, defaulting to identity is required by
	// https://github.com/grpc/grpc/blob/master/doc/compression.md - see test
	// case 6.
	cfg.EnableGzipRequest = o.Enable
}

func (o *gzipOption) applyToHandler(cfg *handlerCfg) {
	cfg.DisableGzipResponse = !o.Enable
}

type interceptOption struct {
	interceptor Interceptor
}

// Intercept configures a client or handler to use the supplied Interceptor.
// Note that this Option replaces any previously-configured Interceptor - to
// compose Interceptors, use a Chain.
func Intercept(interceptor Interceptor) Option {
	return &interceptOption{interceptor}
}

func (o *interceptOption) applyToClient(cfg *clientCfg) {
	cfg.Interceptor = o.interceptor
}

func (o *interceptOption) applyToHandler(cfg *handlerCfg) {
	cfg.Interceptor = o.interceptor
}
