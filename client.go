package rerpc

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

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
	Hooks             *Hooks
}

// A CallOption configures a reRPC client or a single call.
//
// In addition to any options grouped in the documentation below, remember that
// Hooks and Options are also valid CallOptions.
type CallOption interface {
	applyToCall(*callCfg)
}

// NewCall returns the context and CallStreamFunc required to call a remote
// procedure. It's the interface between the reRPC library and the client code
// generated by protoc-gen-go-rerpc; most users won't ever need to deal with it
// directly.
//
// To see an example of how NewCall is used in the generated code, see the
// internal/ping/v1test package.
func NewCall(
	ctx context.Context,
	doer Doer,
	baseURL, pkg, service, method string,
	opts ...CallOption,
) (context.Context, CallStreamFunc) {
	cfg := callCfg{
		Package: pkg,
		Service: service,
		Method:  method,
	}
	for _, opt := range opts {
		opt.applyToCall(&cfg)
	}

	spec := &Specification{
		Package:            cfg.Package,
		Service:            cfg.Service,
		Method:             cfg.Method,
		RequestCompression: CompressionIdentity,
		ReadMaxBytes:       cfg.MaxResponseBytes,
	}
	if cfg.EnableGzipRequest {
		spec.RequestCompression = CompressionGzip
	}
	methodURL := fmt.Sprintf("%s/%s.%s/%s", baseURL, spec.Package, spec.Service, spec.Method)
	if url, err := url.Parse(methodURL); err == nil {
		spec.Path = url.Path
	}
	reqHeader := make(http.Header, 5)
	reqHeader.Set("User-Agent", UserAgent())
	reqHeader.Set("Content-Type", TypeDefaultGRPC)
	reqHeader.Set("Grpc-Encoding", spec.RequestCompression)
	reqHeader.Set("Grpc-Accept-Encoding", acceptEncodingValue) // always advertise identity & gzip
	reqHeader.Set("Te", "trailers")
	ctx = NewCallContext(ctx, *spec, reqHeader, make(http.Header))
	next := CallStreamFunc(func(ctx context.Context) Stream {
		return newClientStream(ctx, doer, methodURL, spec.ReadMaxBytes, cfg.EnableGzipRequest)
	})
	return ctx, next
}
