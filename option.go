package rerpc

import "github.com/rerpc/rerpc/compress"

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

type compressorOption struct {
	Name       string
	Compressor compress.Compressor
}

// Compress configures client and server compression strategies.
//
// For handlers, it registers a compression algorithm. Clients may send
// messages compressed with that algorithm and/or request compressed responses.
// By default, handlers support gzip (using the standard library), compressing
// response messages if the client supports it and the uncompressed message is
// >1KiB.
//
// For clients, registering compressors serves two purposes. First, the client
// asks servers to compress responses using one of the registered algorithms.
// (Note that gRPC's compression negotiation is complex, but most of Google's
// gRPC server implementations won't compress responses unless the request is
// compressed.) Second, it makes all the registered algorithms available for
// use with UseCompressor. Note that actually compressing requests requires
// using both Compressor and UseCompressor.
//
// To remove a previously-registered compressor, re-register the same name with
// a nil compressor.
func Compressor(name string, c compress.Compressor) Option {
	return &compressorOption{
		Name:       name,
		Compressor: c,
	}
}

func (o *compressorOption) applyToClient(cfg *clientCfg) {
	o.apply(cfg.Compressors)
}

func (o *compressorOption) applyToHandler(cfg *handlerCfg) {
	o.apply(cfg.Compressors)
}

func (o *compressorOption) apply(m map[string]compress.Compressor) {
	if o.Compressor == nil {
		delete(m, o.Name)
		return
	}
	m[o.Name] = o.Compressor
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
