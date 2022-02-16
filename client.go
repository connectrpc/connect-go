package connect

import (
	"context"
	"net/http"

	"github.com/bufconnect/connect/codec"
	"github.com/bufconnect/connect/codec/protobuf"
	"github.com/bufconnect/connect/compress"
)

type clientConfiguration struct {
	Protocol          protocol
	Procedure         string
	MaxResponseBytes  int64
	Interceptor       Interceptor
	Compressors       map[string]compress.Compressor
	Codec             codec.Codec
	CodecName         string
	RequestCompressor string
}

func newClientConfiguration(procedure string, options []ClientOption) (*clientConfiguration, *Error) {
	config := clientConfiguration{
		Procedure:   procedure,
		Compressors: make(map[string]compress.Compressor),
	}
	for _, opt := range options {
		opt.applyToClient(&config)
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

func (c *clientConfiguration) Validate() *Error {
	if c.Codec == nil || c.CodecName == "" {
		return Errorf(CodeUnknown, "no codec configured")
	}
	if c.RequestCompressor != "" && c.RequestCompressor != compress.NameIdentity {
		if _, ok := c.Compressors[c.RequestCompressor]; !ok {
			return Errorf(CodeUnknown, "no registered compressor for %q", c.RequestCompressor)
		}
	}
	if c.Protocol == nil {
		return Errorf(CodeUnknown, "no protocol configured")
	}
	return nil
}

func (c *clientConfiguration) Protobuf() codec.Codec {
	if c.CodecName == protobuf.Name {
		return c.Codec
	}
	return protobuf.New()
}

func (c *clientConfiguration) newSpecification(t StreamType) Specification {
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
	applyToClient(*clientConfiguration)
}

type requestCompressorOption struct {
	Name string
}

// WithRequestCompressor configures the client to use the specified algorithm
// to compress request messages. If the algorithm has not been registered using
// WithCompressor, the generated client constructor will return an error.
//
// Because some servers don't support compression, clients default to sending
// uncompressed requests.
func WithRequestCompressor(name string) ClientOption {
	return &requestCompressorOption{Name: name}
}

func (o *requestCompressorOption) applyToClient(config *clientConfiguration) {
	config.RequestCompressor = o.Name
}

type useProtocolOption struct {
	Protocol protocol
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
	options ...ClientOption,
) (func(context.Context) (Sender, Receiver), error) {
	config, err := newClientConfiguration(procedure, options)
	if err != nil {
		return nil, err
	}
	protocolClient, protocolErr := config.Protocol.NewClient(&protocolClientParams{
		Spec:             config.newSpecification(stype),
		CompressorName:   config.RequestCompressor,
		Compressors:      newReadOnlyCompressors(config.Compressors),
		CodecName:        config.CodecName,
		Codec:            config.Codec,
		Protobuf:         config.Protobuf(),
		MaxResponseBytes: config.MaxResponseBytes,
		Doer:             doer,
		BaseURL:          baseURL,
	})
	if protocolErr != nil {
		return nil, Wrap(CodeUnknown, protocolErr)
	}
	return func(ctx context.Context) (Sender, Receiver) {
		if ic := config.Interceptor; ic != nil {
			ctx = ic.WrapContext(ctx)
		}
		header := make(http.Header, 8) // arbitrary power of two, prevent immediate resizing
		protocolClient.WriteRequestHeader(header)
		sender, receiver := protocolClient.NewStream(ctx, header)
		if ic := config.Interceptor; ic != nil {
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
	options ...ClientOption,
) (func(context.Context, *Request[Req]) (*Response[Res], error), error) {
	config, err := newClientConfiguration(procedure, options)
	if err != nil {
		return nil, err
	}
	spec := config.newSpecification(StreamTypeUnary)
	protocolClient, protocolErr := config.Protocol.NewClient(&protocolClientParams{
		Spec:             spec,
		CompressorName:   config.RequestCompressor,
		Compressors:      newReadOnlyCompressors(config.Compressors),
		CodecName:        config.CodecName,
		Codec:            config.Codec,
		Protobuf:         config.Protobuf(),
		MaxResponseBytes: config.MaxResponseBytes,
		Doer:             doer,
		BaseURL:          baseURL,
	})
	if protocolErr != nil {
		return nil, Wrap(CodeUnknown, protocolErr)
	}
	send := Func(func(ctx context.Context, request AnyRequest) (AnyResponse, error) {
		sender, receiver := protocolClient.NewStream(ctx, request.Header())
		if err := sender.Send(request.Any()); err != nil {
			_ = sender.Close(err)
			_ = receiver.Close()
			return nil, err
		}
		if err := sender.Close(nil); err != nil {
			_ = receiver.Close()
			return nil, err
		}
		response, err := ReceiveResponse[Res](receiver)
		if err != nil {
			_ = receiver.Close()
			return nil, err
		}
		return response, receiver.Close()
	})
	if ic := config.Interceptor; ic != nil {
		send = ic.Wrap(send)
	}
	return func(ctx context.Context, request *Request[Req]) (*Response[Res], error) {
		// To make the specification and RPC headers visible to the full interceptor
		// chain (as though they were supplied by the caller), we'll add them here.
		request.spec = spec
		protocolClient.WriteRequestHeader(request.Header())
		response, err := send(ctx, request)
		if err != nil {
			return nil, err
		}
		typed, ok := response.(*Response[Res])
		if !ok {
			return nil, Errorf(CodeInternal, "unexpected client response type %T", response)
		}
		return typed, nil
	}, nil
}
