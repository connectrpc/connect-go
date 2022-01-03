package rerpc

import (
	"context"
	"net/http"
	"net/url"
)

var teTrailersSlice = []string{"trailers"}

// Doer is the transport-level interface reRPC expects HTTP clients to
// implement. The standard library's http.Client implements Doer.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type callCfg struct {
	Package           string
	Service           string
	Method            string
	EnableGzipRequest bool
	MaxResponseBytes  int64
	Interceptor       Interceptor
}

// A CallOption configures a reRPC client or a single call.
//
// In addition to any options grouped in the documentation below, remember that
// Options are also valid CallOptions.
type CallOption interface {
	applyToCall(*callCfg)
}

// NewClientStream returns the context and StreamFunc required to call a
// streaming remote procedure. (To call a unary procedure, use NewClientFunc
// instead.)
//
// It's the interface between the reRPC library and the client code generated
// by protoc-gen-go-rerpc; most users won't ever need to deal with it directly.
// To see an example of how NewCall is used in the generated code, see the
// internal/ping/v1test package.
func NewClientStream(
	ctx context.Context,
	doer Doer,
	stype StreamType,
	baseURL, pkg, service, method string,
	opts ...CallOption,
) (context.Context, StreamFunc) {
	cfg := callCfg{
		Package: pkg,
		Service: service,
		Method:  method,
	}
	for _, opt := range opts {
		opt.applyToCall(&cfg)
	}

	spec := Specification{
		Type:               stype,
		Package:            cfg.Package,
		Service:            cfg.Service,
		Method:             cfg.Method,
		RequestCompression: CompressionIdentity,
	}
	if cfg.EnableGzipRequest {
		spec.RequestCompression = CompressionGzip
	}
	// We don't want to use fmt.Sprintf on the hot path - it allocates a lot for
	// a small gain in readability.
	methodURL := baseURL + "/" + spec.Package + "." + spec.Service + "/" + spec.Method
	if url, err := url.Parse(methodURL); err == nil {
		spec.Path = url.Path
	}
	reqHeader := make(http.Header, 5)
	// We know these header keys are in canonical form, so we can bypass all the
	// checks in Header.Set. To avoid allocating the same slices over and over,
	// we use pre-allocated globals for the header values.
	reqHeader["User-Agent"] = userAgentSlice
	reqHeader["Content-Type"] = typeDefaultGRPCSlice
	reqHeader["Grpc-Encoding"] = compressionToSlice(spec.RequestCompression)
	reqHeader["Grpc-Accept-Encoding"] = acceptEncodingValueSlice // always advertise identity & gzip
	reqHeader["Te"] = teTrailersSlice
	ctx = NewCallContext(ctx, spec, reqHeader, make(http.Header))
	sf := StreamFunc(func(ctx context.Context) Stream {
		return newClientStream(ctx, doer, methodURL, cfg.MaxResponseBytes, cfg.EnableGzipRequest)
	})
	if ic := cfg.Interceptor; ic != nil {
		sf = ic.WrapStream(sf)
	}
	return ctx, sf
}

// NewClientFunc returns a strongly-typed function to call a unary remote
// procedure. (To call a streaming procedure, use NewClientStream instead.)
//
// It's the interface between the reRPC library and the client code generated
// by protoc-gen-go-rerpc; most users won't ever need to deal with it directly.
// To see an example of how NewFunc is used in the generated code, see the
// internal/ping/v1test package.
func NewClientFunc[Req, Res any](
	doer Doer,
	baseURL, pkg, service, method string,
	opts ...CallOption,
) func(context.Context, *Req) (*Res, error) {
	// TODO: factor out portions shared with NewStream
	cfg := callCfg{
		Package: pkg,
		Service: service,
		Method:  method,
	}
	for _, opt := range opts {
		opt.applyToCall(&cfg)
	}

	spec := Specification{
		Type:               StreamTypeUnary,
		Package:            cfg.Package,
		Service:            cfg.Service,
		Method:             cfg.Method,
		RequestCompression: CompressionIdentity,
	}
	if cfg.EnableGzipRequest {
		spec.RequestCompression = CompressionGzip
	}
	// We don't want to use fmt.Sprintf on the hot path - it allocates a lot for
	// a small gain in readability.
	methodURL := baseURL + "/" + spec.Package + "." + spec.Service + "/" + spec.Method
	if url, err := url.Parse(methodURL); err == nil {
		spec.Path = url.Path
	}
	reqHeader := make(http.Header, 5)
	// We know these header keys are in canonical form, so we can bypass all the
	// checks in Header.Set. To avoid allocating the same slices over and over,
	// we use pre-allocated globals for the header values.
	reqHeader["User-Agent"] = userAgentSlice
	reqHeader["Content-Type"] = typeDefaultGRPCSlice
	reqHeader["Grpc-Encoding"] = compressionToSlice(spec.RequestCompression)
	reqHeader["Grpc-Accept-Encoding"] = acceptEncodingValueSlice // always advertise identity & gzip
	reqHeader["Te"] = teTrailersSlice
	f := Func(func(ctx context.Context, msg interface{}) (interface{}, error) {
		stream := newClientStream(ctx, doer, methodURL, cfg.MaxResponseBytes, cfg.EnableGzipRequest)
		if err := stream.Send(msg); err != nil {
			_ = stream.CloseSend(err)
			_ = stream.CloseReceive()
			return nil, err
		}
		if err := stream.CloseSend(nil); err != nil {
			_ = stream.CloseReceive()
			return nil, err
		}
		var res Res
		if err := stream.Receive(&res); err != nil {
			_ = stream.CloseReceive()
			return nil, err
		}
		return &res, stream.CloseReceive()
	})
	if ic := cfg.Interceptor; ic != nil {
		f = ic.Wrap(f)
	}
	return func(ctx context.Context, msg *Req) (*Res, error) {
		ctx = NewCallContext(ctx, spec, reqHeader, make(http.Header))
		res, err := f(ctx, msg)
		if err != nil {
			return nil, err
		}
		typed, ok := res.(*Res)
		if !ok {
			var expected Res
			return nil, Errorf(CodeInternal, "expected response to be *%T, got %T", expected, res)
		}
		return typed, nil
	}
}
