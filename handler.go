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
	"net/http"
)

// A Handler is the server-side implementation of a single RPC defined by a
// protocol buffer service.
//
// By default, Handlers support the gRPC and gRPC-Web protocols with the binary
// protobuf and JSON codecs. They support gzip compression using the standard
// library's compress/gzip.
type Handler struct {
	spec             Specification
	interceptor      Interceptor
	implementation   func(context.Context, Sender, Receiver, error /* client-visible */)
	protocolHandlers []protocolHandler
}

// NewUnaryHandler constructs a Handler for a request-response procedure.
func NewUnaryHandler[Req, Res any](
	procedure, registrationName string,
	unary func(context.Context, *Envelope[Req]) (*Envelope[Res], error),
	options ...HandlerOption,
) *Handler {
	config := newHandlerConfiguration(procedure, registrationName, options)
	// Given a (possibly failed) stream, how should we call the unary function?
	implementation := func(ctx context.Context, sender Sender, receiver Receiver, clientVisibleError error) {
		defer receiver.Close()

		var request *Envelope[Req]
		if clientVisibleError != nil {
			// The protocol implementation failed to establish a stream. To make the
			// resulting error visible to the interceptor stack, we still want to
			// call the wrapped unary Func. To do that safely, we need a useful
			// Message struct. (Note that we do *not* actually calling the handler's
			// implementation.)
			request = receiveUnaryEnvelopeMetadata[Req](receiver)
		} else {
			var err error
			request, err = receiveUnaryEnvelope[Req](receiver)
			if err != nil {
				// Interceptors should see this error too. Just as above, they need a
				// useful Message.
				clientVisibleError = err
				request = receiveUnaryEnvelopeMetadata[Req](receiver)
			}
		}

		untyped := UnaryFunc(func(ctx context.Context, request AnyEnvelope) (AnyEnvelope, error) {
			if clientVisibleError != nil {
				// We've already encountered an error, short-circuit before calling the
				// handler's implementation.
				return nil, clientVisibleError
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			typed, ok := request.(*Envelope[Req])
			if !ok {
				return nil, errorf(CodeInternal, "unexpected handler request type %T", request)
			}
			res, err := unary(ctx, typed)
			if err != nil {
				return nil, err
			}
			// The handler implementation can't set the specification using exported
			// APIs (and shouldn't need to). We set it here so it's visible to the
			// interceptor chain.
			res.spec = config.newSpecification(StreamTypeUnary)
			return res, nil
		})
		if ic := config.Interceptor; ic != nil {
			untyped = ic.WrapUnary(untyped)
		}

		response, err := untyped(ctx, request)
		if err != nil {
			_ = sender.Close(err)
			return
		}
		mergeHeaders(sender.Header(), response.Header())
		mergeHeaders(sender.Trailer(), response.Trailer())
		_ = sender.Close(sender.Send(response.Any()))
	}

	protocolHandlers := config.newProtocolHandlers(StreamTypeUnary)
	return &Handler{
		spec:             config.newSpecification(StreamTypeUnary),
		interceptor:      nil, // already applied
		implementation:   implementation,
		protocolHandlers: protocolHandlers,
	}
}

// NewClientStreamHandler constructs a Handler for a client streaming procedure.
func NewClientStreamHandler[Req, Res any](
	procedure, registrationName string,
	implementation func(context.Context, *ClientStream[Req, Res]) error,
	options ...HandlerOption,
) *Handler {
	return newStreamHandler(
		procedure, registrationName,
		StreamTypeClient,
		func(ctx context.Context, sender Sender, receiver Receiver) {
			stream := NewClientStream[Req, Res](sender, receiver)
			err := implementation(ctx, stream)
			_ = receiver.Close()
			_ = sender.Close(err)
		},
		options...,
	)
}

// NewServerStreamHandler constructs a Handler for a server streaming procedure.
func NewServerStreamHandler[Req, Res any](
	procedure, registrationName string,
	implementation func(context.Context, *Envelope[Req], *ServerStream[Res]) error,
	options ...HandlerOption,
) *Handler {
	return newStreamHandler(
		procedure, registrationName,
		StreamTypeServer,
		func(ctx context.Context, sender Sender, receiver Receiver) {
			stream := NewServerStream[Res](sender)
			req, err := receiveUnaryEnvelope[Req](receiver)
			if err != nil {
				_ = receiver.Close()
				_ = sender.Close(err)
				return
			}
			if err := receiver.Close(); err != nil {
				_ = sender.Close(err)
				return
			}
			err = implementation(ctx, req, stream)
			_ = sender.Close(err)
		},
		options...,
	)
}

// NewBidiStreamHandler constructs a Handler for a bidirectional streaming procedure.
func NewBidiStreamHandler[Req, Res any](
	procedure, registrationName string,
	implementation func(context.Context, *BidiStream[Req, Res]) error,
	options ...HandlerOption,
) *Handler {
	return newStreamHandler(
		procedure, registrationName,
		StreamTypeBidirectional,
		func(ctx context.Context, sender Sender, receiver Receiver) {
			stream := NewBidiStream[Req, Res](sender, receiver)
			err := implementation(ctx, stream)
			_ = receiver.Close()
			_ = sender.Close(err)
		},
		options...,
	)
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// We don't need to defer functions  to close the request body or read to
	// EOF: the stream we construct later on already does that, and we only
	// return early when dealing with misbehaving clients. In those cases, it's
	// okay if we can't re-use the connection.
	isBidi := (h.spec.StreamType & StreamTypeBidirectional) == StreamTypeBidirectional
	if isBidi && r.ProtoMajor < 2 {
		h.failNegotiation(w, http.StatusHTTPVersionNotSupported)
		return
	}

	methodHandlers := make([]protocolHandler, 0, len(h.protocolHandlers))
	for _, protocolHandler := range h.protocolHandlers {
		if protocolHandler.ShouldHandleMethod(r.Method) {
			methodHandlers = append(methodHandlers, protocolHandler)
		}
	}
	if len(methodHandlers) == 0 {
		// grpc-go returns a 500 here, but interoperability with non-gRPC HTTP
		// clients is better if we return a 405.
		h.failNegotiation(w, http.StatusMethodNotAllowed)
		return
	}

	// TODO: for GETs, we should parse the Accept header and offer each handler
	// each content-type.
	contentType := r.Header.Get("Content-Type")
	for _, protocolHandler := range methodHandlers {
		if !protocolHandler.ShouldHandleContentType(contentType) {
			continue
		}
		ctx := r.Context()
		if ic := h.interceptor; ic != nil {
			ctx = ic.WrapStreamContext(ctx)
		}
		// Most errors returned from protocolHandler.NewStream are caused by
		// invalid requests. For example, the client may have specified an invalid
		// timeout or an unavailable codec. We'd like those errors to be visible to
		// the interceptor chain, so we're going to capture them here and pass them
		// to the implementation.
		sender, receiver, clientVisibleError := protocolHandler.NewStream(w, r.WithContext(ctx))
		// If NewStream errored and the protocol doesn't want the error sent to
		// the client, sender and/or receiver may be nil. We still want the
		// error to be seen by interceptors, so we provide no-op Sender and
		// Receiver implementations.
		if clientVisibleError != nil && sender == nil {
			sender = newNopSender(h.spec, w.Header(), make(http.Header))
		}
		if clientVisibleError != nil && receiver == nil {
			receiver = newNopReceiver(h.spec, r.Header, r.Trailer)
		}
		if ic := h.interceptor; ic != nil {
			// Unary interceptors were handled in NewUnaryHandler.
			sender = ic.WrapStreamSender(ctx, sender)
			receiver = ic.WrapStreamReceiver(ctx, receiver)
		}
		h.implementation(ctx, sender, receiver, clientVisibleError)
		return
	}
	h.failNegotiation(w, http.StatusUnsupportedMediaType)
}

func (h *Handler) failNegotiation(w http.ResponseWriter, code int) {
	// None of the registered protocols is able to serve the request.
	for _, ph := range h.protocolHandlers {
		ph.WriteAccept(w.Header())
	}
	w.WriteHeader(code)
}

type handlerConfiguration struct {
	CompressionPools map[string]compressionPool
	Codecs           map[string]Codec
	MaxRequestBytes  int64
	CompressMinBytes int
	Registrar        *Registrar
	Interceptor      Interceptor
	Procedure        string
	RegistrationName string
	HandleGRPC       bool
	HandleGRPCWeb    bool
}

func newHandlerConfiguration(procedure, registrationName string, options []HandlerOption) *handlerConfiguration {
	config := handlerConfiguration{
		Procedure:        procedure,
		RegistrationName: registrationName,
		CompressionPools: make(map[string]compressionPool),
		Codecs:           make(map[string]Codec),
		HandleGRPC:       true,
		HandleGRPCWeb:    true,
	}
	WithProtoBinaryCodec().applyToHandler(&config)
	WithProtoJSONCodec().applyToHandler(&config)
	WithGzip().applyToHandler(&config)
	for _, opt := range options {
		opt.applyToHandler(&config)
	}
	if reg := config.Registrar; reg != nil && config.RegistrationName != "" {
		reg.register(config.RegistrationName)
	}
	return &config
}

func (c *handlerConfiguration) newSpecification(streamType StreamType) Specification {
	return Specification{
		Procedure:  c.Procedure,
		StreamType: streamType,
	}
}

func (c *handlerConfiguration) newProtocolHandlers(streamType StreamType) []protocolHandler {
	var protocols []protocol
	if c.HandleGRPC {
		protocols = append(protocols, &protocolGRPC{web: false})
	}
	if c.HandleGRPCWeb {
		protocols = append(protocols, &protocolGRPC{web: true})
	}
	handlers := make([]protocolHandler, 0, len(protocols))
	codecs := newReadOnlyCodecs(c.Codecs)
	compressors := newReadOnlyCompressionPools(c.CompressionPools)
	for _, protocol := range protocols {
		handlers = append(handlers, protocol.NewHandler(&protocolHandlerParams{
			Spec:             c.newSpecification(streamType),
			Codecs:           codecs,
			CompressionPools: compressors,
			MaxRequestBytes:  c.MaxRequestBytes,
			CompressMinBytes: c.CompressMinBytes,
		}))
	}
	return handlers
}

func newStreamHandler(
	procedure, registrationName string,
	streamType StreamType,
	implementation func(context.Context, Sender, Receiver),
	options ...HandlerOption,
) *Handler {
	config := newHandlerConfiguration(procedure, registrationName, options)
	return &Handler{
		spec:        config.newSpecification(streamType),
		interceptor: config.Interceptor,
		implementation: func(ctx context.Context, sender Sender, receiver Receiver, clientVisibleErr error) {
			if clientVisibleErr != nil {
				_ = receiver.Close()
				_ = sender.Close(clientVisibleErr)
				return
			}
			implementation(ctx, sender, receiver)
		},
		protocolHandlers: config.newProtocolHandlers(streamType),
	}
}
