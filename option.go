package rerpc

// Option implements both CallOption and HandlerOption, so it can be applied
// both client-side and server-side.
type Option interface {
	CallOption
	HandlerOption
}

type overrideProtobufTypes struct {
	pkg, svc string
}

// OverrideProtobufTypes replaces the protobuf package and service names set by
// the generated code. This affects URLs and any Specification retrieved from a
// call or handler context. This is usually a bad idea, but it's occasionally
// necessary to prevent protobuf package collisions. (For example, reRPC uses
// this option to serve the health and reflection APIs without generating
// runtime conflicts with grpc-go.)
//
// It does not change the data exposed by the reflection API. To prevent
// inconsistencies, using this option disables reflection for the service
// (though other services can still be introspected).
func OverrideProtobufTypes(pkg, service string) Option {
	return &overrideProtobufTypes{pkg, service}
}

func (o *overrideProtobufTypes) applyToCall(cfg *callCfg) {
	cfg.Package = o.pkg
	cfg.Service = o.svc
}

func (o *overrideProtobufTypes) applyToHandler(cfg *handlerCfg) {
	cfg.Package = o.pkg
	cfg.Service = o.svc
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

func (o *readMaxBytes) applyToCall(cfg *callCfg) {
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
// For handlers, enabling gzip sends compressed responses to clients that
// support them. Handlers default to using gzip where possible.
//
// For clients, enabling gzip compresses requests. ReRPC clients always ask for
// compressed responses (even if the request is uncompressed), but most gRPC
// servers only support symmetric compression: they'll only gzip the response
// if the client sends a gzipped request. Since not all servers support gzip
// compression, clients default to sending uncompressed requests.
func Gzip(enable bool) Option {
	return &gzipOption{enable}
}

func (o *gzipOption) applyToCall(cfg *callCfg) {
	// NB, the default is required by
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

func (o *interceptOption) applyToCall(cfg *callCfg) {
	cfg.Interceptor = o.interceptor
}

func (o *interceptOption) applyToHandler(cfg *handlerCfg) {
	cfg.Interceptor = o.interceptor
}
