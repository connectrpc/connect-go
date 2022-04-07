// Copyright 2021-2022 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connect

import (
	"context"
	"errors"
	"io"
	"net/http"
)

// Client is a reusable, concurrency-safe client for a single procedure.
// Depending on the procedure's type, use the CallUnary, CallClientStream,
// CallServerStream, or CallBidiStream method.
//
// By default, clients use the binary Protobuf Codec, ask for gzipped
// responses, and send uncompressed requests. They don't have a default
// protocol; callers of NewClient or generated client constructors must
// explicitly choose a protocol with either the WithGRPC or WithGRPCWeb
// options.
type Client[Req, Res any] struct {
	config         *clientConfiguration
	callUnary      func(context.Context, *Request[Req]) (*Response[Res], error)
	protocolClient protocolClient
	warnIfError    func(error)
}

// NewClient constructs a new Client.
func NewClient[Req, Res any](
	httpClient HTTPClient,
	url string,
	options ...ClientOption,
) (*Client[Req, Res], error) {
	config, err := newClientConfiguration(url, options)
	if err != nil {
		return nil, err
	}
	warnIfError := newWarnIfError(config.Warn)
	protocolClient, protocolErr := config.Protocol.NewClient(&protocolClientParams{
		CompressionName:  config.RequestCompressionName,
		CompressionPools: newReadOnlyCompressionPools(config.CompressionPools),
		Codec:            config.Codec,
		Protobuf:         config.protobuf(),
		CompressMinBytes: config.CompressMinBytes,
		HTTPClient:       httpClient,
		URL:              url,
	})
	if protocolErr != nil {
		return nil, protocolErr
	}
	// Rather than applying unary interceptors along the hot path, we can do it
	// once at client creation.
	unarySpec := config.newSpecification(StreamTypeUnary)
	unaryFunc := UnaryFunc(func(ctx context.Context, request AnyRequest) (AnyResponse, error) {
		sender, receiver := protocolClient.NewStream(ctx, unarySpec, request.Header())
		// Send always returns an io.EOF unless the error is from the client-side.
		// We want the user to continue to call Receive in those cases to get the
		// full error from the server-side.
		if err := sender.Send(request.Any()); err != nil && !errors.Is(err, io.EOF) {
			warnIfError(sender.Close(err))
			warnIfError(receiver.Close())
			return nil, err
		}
		if err := sender.Close(nil); err != nil {
			warnIfError(receiver.Close())
			return nil, err
		}
		response, err := receiveUnaryResponse[Res](receiver)
		if err != nil {
			warnIfError(receiver.Close())
			return nil, err
		}
		return response, receiver.Close()
	})
	if ic := config.Interceptor; ic != nil {
		unaryFunc = ic.WrapUnary(unaryFunc)
	}
	callUnary := func(ctx context.Context, request *Request[Req]) (*Response[Res], error) {
		// To make the specification and RPC headers visible to the full interceptor
		// chain (as though they were supplied by the caller), we'll add them here.
		request.spec = unarySpec
		protocolClient.WriteRequestHeader(request.Header())
		response, err := unaryFunc(ctx, request)
		if err != nil {
			return nil, err
		}
		typed, ok := response.(*Response[Res])
		if !ok {
			return nil, errorf(CodeInternal, "unexpected client response type %T", response)
		}
		return typed, nil
	}
	return &Client[Req, Res]{
		config:         config,
		callUnary:      callUnary,
		protocolClient: protocolClient,
		warnIfError:    warnIfError,
	}, nil
}

// CallUnary calls a request-response procedure.
func (c *Client[Req, Res]) CallUnary(
	ctx context.Context,
	req *Request[Req],
) (*Response[Res], error) {
	return c.callUnary(ctx, req)
}

// CallClientStream calls a client streaming procedure.
func (c *Client[Req, Res]) CallClientStream(ctx context.Context) *ClientStreamForClient[Req, Res] {
	sender, receiver := c.newStream(ctx, StreamTypeClient)
	return NewClientStreamForClient[Req, Res](sender, receiver)
}

// CallServerStream calls a server streaming procedure.
func (c *Client[Req, Res]) CallServerStream(
	ctx context.Context,
	req *Request[Req],
) (*ServerStreamForClient[Res], error) {
	sender, receiver := c.newStream(ctx, StreamTypeServer)
	mergeHeaders(sender.Header(), req.header)
	// Send always returns an io.EOF unless the error is from the client-side.
	// We want the user to continue to call Receive in those cases to get the
	// full error from the server-side.
	if err := sender.Send(req.Msg); err != nil && !errors.Is(err, io.EOF) {
		c.warnIfError(sender.Close(err))
		c.warnIfError(receiver.Close())
		return nil, err
	}
	if err := sender.Close(nil); err != nil {
		return nil, err
	}
	return NewServerStreamForClient[Res](receiver), nil
}

// CallBidiStream calls a bidirectional streaming procedure.
func (c *Client[Req, Res]) CallBidiStream(ctx context.Context) *BidiStreamForClient[Req, Res] {
	sender, receiver := c.newStream(ctx, StreamTypeBidi)
	return NewBidiStreamForClient[Req, Res](sender, receiver)
}

func (c *Client[Req, Res]) newStream(ctx context.Context, streamType StreamType) (Sender, Receiver) {
	if ic := c.config.Interceptor; ic != nil {
		ctx = ic.WrapStreamContext(ctx)
	}
	header := make(http.Header, 8) // arbitrary power of two, prevent immediate resizing
	c.protocolClient.WriteRequestHeader(header)
	sender, receiver := c.protocolClient.NewStream(ctx, c.config.newSpecification(streamType), header)
	if ic := c.config.Interceptor; ic != nil {
		sender = ic.WrapStreamSender(ctx, sender)
		receiver = ic.WrapStreamReceiver(ctx, receiver)
	}
	return sender, receiver
}

type clientConfiguration struct {
	Protocol               protocol
	Procedure              string
	CompressMinBytes       int
	Interceptor            Interceptor
	CompressionPools       map[string]compressionPool
	Codec                  Codec
	RequestCompressionName string
	Warn                   func(error)
}

func newClientConfiguration(url string, options []ClientOption) (*clientConfiguration, *Error) {
	protoPath := extractProtobufPath(url)
	config := clientConfiguration{
		Procedure:        protoPath,
		CompressionPools: make(map[string]compressionPool),
		Warn:             defaultWarn,
	}
	WithProtoBinaryCodec().applyToClient(&config)
	WithGzip().applyToClient(&config)
	for _, opt := range options {
		opt.applyToClient(&config)
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

func (c *clientConfiguration) validate() *Error {
	if c.Codec == nil || c.Codec.Name() == "" {
		return errorf(CodeUnknown, "no codec configured")
	}
	if c.RequestCompressionName != "" && c.RequestCompressionName != compressionIdentity {
		if _, ok := c.CompressionPools[c.RequestCompressionName]; !ok {
			return errorf(CodeUnknown, "unknown compression %q", c.RequestCompressionName)
		}
	}
	if c.Protocol == nil {
		return errorf(
			CodeUnknown,
			"no protocol configured: use either WithGRPC() or WithGRPCWeb()",
		)
	}
	return nil
}

func (c *clientConfiguration) protobuf() Codec {
	if c.Codec.Name() == codecNameProto {
		return c.Codec
	}
	return &protoBinaryCodec{}
}

func (c *clientConfiguration) newSpecification(t StreamType) Specification {
	return Specification{
		StreamType: t,
		Procedure:  c.Procedure,
		IsClient:   true,
	}
}
