package rerpc

import (
	"context"
	"fmt"
	"net/http"

	"github.com/rerpc/rerpc/compress"
)

var teTrailersSlice = []string{"trailers"}

// Doer is the transport-level interface reRPC expects HTTP clients to
// implement. The standard library's http.Client implements Doer.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type clientCfg struct {
	Package           string
	Service           string
	Method            string
	MaxResponseBytes  int64
	Interceptor       Interceptor
	Compressors       map[string]compress.Compressor
	RequestCompressor string
}

// A ClientOption configures a reRPC client.
//
// In addition to any options grouped in the documentation below, remember that
// Options are also valid ClientOptions.
type ClientOption interface {
	applyToClient(*clientCfg)
}

type useCompressorOption struct {
	Name string
}

// UseCompressor configures the client to use the specified algorithm to
// compress request messages. If the algorithm has not been registered using
// Compressor, the request message is sent uncompressed.
//
// Because some servers don't support compression, clients default to sending
// uncompressed requests.
func UseCompressor(name string) ClientOption {
	return &useCompressorOption{Name: name}
}

func (o *useCompressorOption) applyToClient(cfg *clientCfg) {
	cfg.RequestCompressor = o.Name
}

// NewClientStream returns the context and StreamFunc required to call a
// streaming remote procedure. (To call a unary procedure, use NewClientFunc
// instead.)
//
// It's the interface between the reRPC library and the client code generated
// by protoc-gen-go-rerpc; most users won't ever need to deal with it directly.
// To see an example of how NewClientStream is used in the generated code, see the
// internal/ping/v1test package.
func NewClientStream(
	doer Doer,
	stype StreamType,
	baseURL, pkg, service, method string,
	opts ...ClientOption,
) StreamFunc {
	cfg := clientCfg{
		Package: pkg,
		Service: service,
		Method:  method,
		// NB, defaulting RequestCompressor to identity is required by
		// https://github.com/grpc/grpc/blob/master/doc/compression.md - see test
		// case 6.
	}
	for _, opt := range opts {
		opt.applyToClient(&cfg)
	}
	compressors := newROCompressors(cfg.Compressors)
	if !compressors.Contains(cfg.RequestCompressor) {
		cfg.RequestCompressor = ""
	}
	procedure := fmt.Sprintf("%s.%s/%s", cfg.Package, cfg.Service, cfg.Method)
	spec := Specification{
		Type:      stype,
		Procedure: procedure,
		IsClient:  true,
	}
	sf := StreamFunc(func(ctx context.Context) (context.Context, Stream) {
		header := Header{raw: make(http.Header, 8)}
		addGRPCClientHeaders(header, compressors.Names(), cfg.RequestCompressor)
		return ctx, newClientStream(
			ctx,
			doer,
			baseURL,
			spec,
			header,
			cfg.MaxResponseBytes,
			compressors.Get(cfg.RequestCompressor),
			compressors,
		)
	})
	if ic := cfg.Interceptor; ic != nil {
		sf = ic.WrapStream(sf)
	}
	return sf
}

// NewClientFunc returns a strongly-typed function to call a unary remote
// procedure. (To call a streaming procedure, use NewClientStream instead.)
//
// It's the interface between the reRPC library and the client code generated
// by protoc-gen-go-rerpc; most users won't ever need to deal with it directly.
// To see an example of how NewClientFunc is used in the generated code, see the
// internal/ping/v1test package.
func NewClientFunc[Req, Res any](
	doer Doer,
	baseURL, pkg, service, method string,
	opts ...ClientOption,
) func(context.Context, *Request[Req]) (*Response[Res], error) {
	cfg := clientCfg{
		Package: pkg,
		Service: service,
		Method:  method,
	}
	for _, opt := range opts {
		opt.applyToClient(&cfg)
	}
	compressors := newROCompressors(cfg.Compressors)
	if !compressors.Contains(cfg.RequestCompressor) {
		cfg.RequestCompressor = ""
	}
	procedure := fmt.Sprintf("%s.%s/%s", cfg.Package, cfg.Service, cfg.Method)
	spec := Specification{
		Type:      StreamTypeUnary,
		Procedure: procedure,
		IsClient:  true,
	}
	send := Func(func(ctx context.Context, msg AnyRequest) (AnyResponse, error) {
		stream := newClientStream(
			ctx,
			doer,
			baseURL,
			spec,
			msg.Header(),
			cfg.MaxResponseBytes,
			compressors.Get(cfg.RequestCompressor),
			compressors,
		)
		if err := stream.Send(msg.Any()); err != nil {
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
		return newResponseWithHeader(&res, stream.ReceivedHeader()), stream.CloseReceive()
	})
	// To make the specification and RPC headers visible to the full interceptor
	// chain (as though they were supplied by the caller), we'll add them in the
	// outermost interceptor.
	preparer := func(next Func) Func {
		return func(ctx context.Context, req AnyRequest) (AnyResponse, error) {
			typed, ok := req.(*Request[Req])
			if !ok {
				return nil, Errorf(CodeInternal, "unexpected client request type %T", req)
			}
			typed.spec = spec
			addGRPCClientHeaders(req.Header(), compressors.Names(), cfg.RequestCompressor)
			return next(ctx, typed)
		}
	}
	wrapped := NewChain(
		UnaryInterceptorFunc(preparer),
		cfg.Interceptor,
	).Wrap(send)
	return func(ctx context.Context, msg *Request[Req]) (*Response[Res], error) {
		res, err := wrapped(ctx, msg)
		if err != nil {
			return nil, err
		}
		typed, ok := res.(*Response[Res])
		if !ok {
			return nil, Errorf(CodeInternal, "unexpected client response type %T", res)
		}
		return typed, nil
	}
}

func addGRPCClientHeaders(h Header, acceptCompression string, requestCompressor string) {
	// We know these header keys are in canonical form, so we can bypass all the
	// checks in Header.Set. To avoid allocating the same slices over and over,
	// we use pre-allocated globals for the header values.
	h.raw["User-Agent"] = []string{userAgent}
	h.raw["Content-Type"] = []string{TypeProtoGRPC}
	if requestCompressor != "" && requestCompressor != compress.NameIdentity {
		h.raw["Grpc-Encoding"] = []string{requestCompressor}
	}
	if acceptCompression != "" {
		h.raw["Grpc-Accept-Encoding"] = []string{acceptCompression}
	}
	h.raw["Te"] = teTrailersSlice
}
