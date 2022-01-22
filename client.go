package rerpc

import (
	"context"
	"net/http"

	"github.com/rerpc/rerpc/codec"
	"github.com/rerpc/rerpc/codec/protobuf"
	"github.com/rerpc/rerpc/compress"
)

// Doer is the transport-level interface reRPC expects HTTP clients to
// implement. The standard library's http.Client implements Doer.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type clientCfg struct {
	Protocol          protocol
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
		Protocol: &grpc{},
	}
	for _, opt := range opts {
		opt.applyToClient(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	spec := Specification{
		Type:      stype,
		Procedure: cfg.Procedure,
		IsClient:  true,
	}
	pclient, err := cfg.Protocol.NewClient(&protocolClientParams{
		Spec:             spec,
		CompressorName:   cfg.RequestCompressor,
		Compressors:      newROCompressors(cfg.Compressors),
		CodecName:        cfg.CodecName,
		Codec:            cfg.Codec,
		Protobuf:         cfg.Protobuf(),
		MaxResponseBytes: cfg.MaxResponseBytes,
		Doer:             doer,
		BaseURL:          baseURL,
	})
	if err != nil {
		return nil, Wrap(CodeUnknown, err)
	}
	// If the protocol can't construct a stream, capture the error here.
	var streamConstructionError error
	sf := StreamFunc(func(ctx context.Context) (context.Context, Sender, Receiver) {
		header := make(http.Header, 8) // arbitrary power of two, avoid immediate resizing
		pclient.WriteRequestHeader(header)
		sender, receiver, err := pclient.NewStream(ctx, Header{raw: header})
		if err != nil {
			streamConstructionError = err
			return ctx,
				newNopSender(spec, Header{raw: header}),
				newNopReceiver(spec, Header{raw: make(http.Header)})
		}
		return ctx, sender, receiver
	})
	if ic := cfg.Interceptor; ic != nil {
		sf = ic.WrapStream(sf)
	}
	return sf, streamConstructionError
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
		Protocol: &grpc{},
	}
	for _, opt := range opts {
		opt.applyToClient(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	spec := Specification{
		Type:      StreamTypeUnary,
		Procedure: cfg.Procedure,
		IsClient:  true,
	}
	pclient, err := cfg.Protocol.NewClient(&protocolClientParams{
		Spec:             spec,
		CompressorName:   cfg.RequestCompressor,
		Compressors:      newROCompressors(cfg.Compressors),
		CodecName:        cfg.CodecName,
		Codec:            cfg.Codec,
		Protobuf:         cfg.Protobuf(),
		MaxResponseBytes: cfg.MaxResponseBytes,
		Doer:             doer,
		BaseURL:          baseURL,
	})
	if err != nil {
		return nil, Wrap(CodeUnknown, err)
	}
	send := Func(func(ctx context.Context, msg AnyRequest) (AnyResponse, error) {
		sender, receiver, err := pclient.NewStream(ctx, msg.Header())
		if err != nil {
			return nil, err
		}
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
	if ic := cfg.Interceptor; ic != nil {
		send = ic.Wrap(send)
	}
	return func(ctx context.Context, msg *Request[Req]) (*Response[Res], error) {
		// To make the specification and RPC headers visible to the full interceptor
		// chain (as though they were supplied by the caller), we'll add them here.
		msg.spec = spec
		pclient.WriteRequestHeader(msg.Header().raw)
		res, err := send(ctx, msg)
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
