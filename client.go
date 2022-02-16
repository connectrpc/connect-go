package connect

import (
	"context"
	"net/http"

	"github.com/bufconnect/connect/codec"
	"github.com/bufconnect/connect/codec/protobuf"
	"github.com/bufconnect/connect/compress"
	"github.com/bufconnect/connect/compress/gzip"
)

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

func newClientConfiguration(procedure string, opts []ClientOption) (*clientCfg, *Error) {
	cfg := clientCfg{
		Procedure: procedure,
		Compressors: map[string]compress.Compressor{
			gzip.Name: gzip.New(),
		},
		Protocol: &grpc{},
	}
	for _, opt := range opts {
		opt.applyToClient(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
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

func (c *clientCfg) newSpecification(t StreamType) Specification {
	return Specification{
		Type:      t,
		Procedure: c.Procedure,
		IsClient:  true,
	}
}

// A ClientOption configures a connect client.
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

type useProtocolOption struct {
	Protocol protocol
}

// UseGRPCWeb configures the client to use the gRPC-Web protocol, rather than
// the default HTTP/2 gRPC variant.
func UseGRPCWeb() ClientOption {
	return &useProtocolOption{Protocol: &grpc{web: true}}
}

func (o *useProtocolOption) applyToClient(c *clientCfg) {
	c.Protocol = o.Protocol
}

// NewClientStream returns a stream constructor for a client-, server-, or
// bidirectional streaming remote procedure. (To call a unary procedure, use
// NewClientFunc instead.)
//
// It's the interface between the connect library and the client code generated
// by protoc-gen-go-connect; most users won't ever need to deal with it directly.
// To see an example of how NewClientStream is used in the generated code, see the
// internal/gen/proto/go-connect/connect/ping/v1test package.
func NewClientStream(
	doer Doer,
	stype StreamType,
	baseURL, procedure string,
	opts ...ClientOption,
) (func(context.Context) (Sender, Receiver), error) {
	cfg, err := newClientConfiguration(procedure, opts)
	if err != nil {
		return nil, err
	}
	spec := cfg.newSpecification(stype)
	pclient, perr := cfg.Protocol.NewClient(&protocolClientParams{
		Spec:             spec,
		CompressorName:   cfg.RequestCompressor,
		Compressors:      newReadOnlyCompressors(cfg.Compressors),
		CodecName:        cfg.CodecName,
		Codec:            cfg.Codec,
		Protobuf:         cfg.Protobuf(),
		MaxResponseBytes: cfg.MaxResponseBytes,
		Doer:             doer,
		BaseURL:          baseURL,
	})
	if perr != nil {
		return nil, Wrap(CodeUnknown, perr)
	}
	return func(ctx context.Context) (Sender, Receiver) {
		if ic := cfg.Interceptor; ic != nil {
			ctx = ic.WrapContext(ctx)
		}
		h := make(http.Header, 8) // arbitrary power of two, prevent immediate resizing
		pclient.WriteRequestHeader(h)
		sender, receiver := pclient.NewStream(ctx, h)
		if ic := cfg.Interceptor; ic != nil {
			sender = ic.WrapSender(ctx, sender)
			receiver = ic.WrapReceiver(ctx, receiver)
		}
		return sender, receiver
	}, nil
}

// NewClientFunc returns a strongly-typed function to call a unary remote
// procedure. (To call a streaming procedure, use NewClientStream instead.)
//
// It's the interface between the connect library and the client code generated
// by protoc-gen-go-connect; most users won't ever need to deal with it directly.
// To see an example of how NewClientFunc is used in the generated code, see the
// internal/gen/proto/go-connect/connect/ping/v1test package.
func NewClientFunc[Req, Res any](
	doer Doer,
	baseURL, procedure string,
	opts ...ClientOption,
) (func(context.Context, *Request[Req]) (*Response[Res], error), error) {
	cfg, err := newClientConfiguration(procedure, opts)
	if err != nil {
		return nil, err
	}
	spec := cfg.newSpecification(StreamTypeUnary)
	pclient, perr := cfg.Protocol.NewClient(&protocolClientParams{
		Spec:             spec,
		CompressorName:   cfg.RequestCompressor,
		Compressors:      newReadOnlyCompressors(cfg.Compressors),
		CodecName:        cfg.CodecName,
		Codec:            cfg.Codec,
		Protobuf:         cfg.Protobuf(),
		MaxResponseBytes: cfg.MaxResponseBytes,
		Doer:             doer,
		BaseURL:          baseURL,
	})
	if perr != nil {
		return nil, Wrap(CodeUnknown, perr)
	}
	send := Func(func(ctx context.Context, msg AnyRequest) (AnyResponse, error) {
		sender, receiver := pclient.NewStream(ctx, msg.Header())
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
		pclient.WriteRequestHeader(msg.Header())
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
