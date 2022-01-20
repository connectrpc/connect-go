package rerpc

import (
	"context"
	"net/http"

	"github.com/rerpc/rerpc/codec"
	"github.com/rerpc/rerpc/codec/protobuf"
	"github.com/rerpc/rerpc/compress"
)

var teTrailersSlice = []string{"trailers"}

// Doer is the transport-level interface reRPC expects HTTP clients to
// implement. The standard library's http.Client implements Doer.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type clientCfg struct {
	Procedure         string
	MaxResponseBytes  int64
	Interceptor       Interceptor
	Compressors       map[string]compress.Compressor
	Codec             codec.Codec
	CodecName         string
	RequestCompressor string
}

func (c *clientCfg) Validate() *Error {
	if c.Codec == nil || c.CodecName == "" {
		return Errorf(CodeUnknown, "no codec configured")
	}
	if c.RequestCompressor != "" && c.RequestCompressor != compress.NameIdentity {
		if _, ok := c.Compressors[c.RequestCompressor]; !ok {
			return Errorf(CodeUnknown, "no registered compressor for %q", c.RequestCompressor)
		}
	}
	return nil
}

func (c *clientCfg) Protobuf() codec.Codec {
	if c.CodecName == protobuf.NameBinary {
		return c.Codec
	}
	return protobuf.NewBinary()
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
	baseURL, procedure string,
	opts ...ClientOption,
) (StreamFunc, error) {
	cfg := clientCfg{
		Procedure: procedure,
		Compressors: map[string]compress.Compressor{
			"gzip": compress.NewGzip(),
		},
		// NB, defaulting RequestCompressor to identity is required by
		// https://github.com/grpc/grpc/blob/master/doc/compression.md - see test
		// case 6.
	}
	for _, opt := range opts {
		opt.applyToClient(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	compressors := newROCompressors(cfg.Compressors)
	spec := Specification{
		Type:      stype,
		Procedure: cfg.Procedure,
		IsClient:  true,
	}
	sf := StreamFunc(func(ctx context.Context) (context.Context, Sender, Receiver) {
		header := Header{raw: make(http.Header, 8)}
		addGRPCClientHeaders(header, cfg.CodecName, compressors.Names(), cfg.RequestCompressor)
		sender, receiver := newClientStream(
			ctx,
			doer,
			baseURL,
			spec,
			header,
			cfg.MaxResponseBytes,
			cfg.Codec,
			cfg.Protobuf(),
			compressors.Get(cfg.RequestCompressor),
			compressors,
		)
		return ctx, sender, receiver
	})
	if ic := cfg.Interceptor; ic != nil {
		sf = ic.WrapStream(sf)
	}
	return sf, nil
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
	baseURL, procedure string,
	opts ...ClientOption,
) (func(context.Context, *Request[Req]) (*Response[Res], error), error) {
	cfg := clientCfg{
		Procedure: procedure,
		Compressors: map[string]compress.Compressor{
			"gzip": compress.NewGzip(),
		},
	}
	for _, opt := range opts {
		opt.applyToClient(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	compressors := newROCompressors(cfg.Compressors)
	spec := Specification{
		Type:      StreamTypeUnary,
		Procedure: cfg.Procedure,
		IsClient:  true,
	}
	send := Func(func(ctx context.Context, msg AnyRequest) (AnyResponse, error) {
		sender, receiver := newClientStream(
			ctx,
			doer,
			baseURL,
			spec,
			msg.Header(),
			cfg.MaxResponseBytes,
			cfg.Codec,
			cfg.Protobuf(),
			compressors.Get(cfg.RequestCompressor),
			compressors,
		)
		if err := sender.Send(msg.Any()); err != nil {
			_ = sender.Close(err)
			_ = receiver.Close()
			return nil, err
		}
		if err := sender.Close(nil); err != nil {
			_ = receiver.Close()
			return nil, err
		}
		res, err := ReceiveResponse[Res](receiver)
		if err != nil {
			_ = receiver.Close()
			return nil, err
		}
		return res, receiver.Close()
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
			addGRPCClientHeaders(req.Header(), cfg.CodecName, compressors.Names(), cfg.RequestCompressor)
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
	}, nil
}

func addGRPCClientHeaders(h Header, codecName, acceptCompression, requestCompressor string) {
	// We know these header keys are in canonical form, so we can bypass all the
	// checks in Header.Set. To avoid allocating the same slices over and over,
	// we use pre-allocated globals for the header values.
	h.raw["User-Agent"] = []string{userAgent}
	h.raw["Content-Type"] = []string{contentTypeFromCodecName(codecName)}
	if requestCompressor != "" && requestCompressor != compress.NameIdentity {
		h.raw["Grpc-Encoding"] = []string{requestCompressor}
	}
	if acceptCompression != "" {
		h.raw["Grpc-Accept-Encoding"] = []string{acceptCompression}
	}
	h.raw["Te"] = teTrailersSlice
}
