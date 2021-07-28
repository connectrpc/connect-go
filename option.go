package rerpc

// Option implements both CallOption and HandlerOption, so it can be applied
// both client-side and server-side.
type Option interface {
	CallOption
	HandlerOption
}

type readMaxBytes struct {
	Max int
}

// ReadMaxBytes limits the performance impact of pathologically large messages
// sent by the other party. For handlers, ReadMaxBytes sets the maximum
// allowable request size. For clients, ReadMaxBytes sets the maximum allowable
// response size. Limits are applied before decompression.
//
// Setting ReadMaxBytes to zero allows any request size. Both clients and
// handlers default to allowing any request size.
func ReadMaxBytes(n int) Option {
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
