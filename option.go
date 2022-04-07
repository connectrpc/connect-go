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
	"compress/gzip"
	"io/ioutil"
)

// A ClientOption configures a connect client.
//
// In addition to any options grouped in the documentation below, remember that
// Options are also valid ClientOptions.
type ClientOption interface {
	applyToClient(*clientConfiguration)
}

// WithClientOptions composes multiple ClientOptions into one.
func WithClientOptions(options ...ClientOption) ClientOption {
	return &clientOptionsOption{options}
}

// WithGRPC configures clients to use the HTTP/2 gRPC protocol.
func WithGRPC() ClientOption {
	return &grpcOption{web: false}
}

// WithGRPCWeb configures clients to use the gRPC-Web protocol.
func WithGRPCWeb() ClientOption {
	return &grpcOption{web: true}
}

// WithGzipRequests configures the client to gzip requests. It requires that
// the client already have a registered gzip compressor (via either WithGzip or
// WithCompressor).
//
// Because some servers don't support gzip, clients default to sending
// uncompressed requests.
func WithGzipRequests() ClientOption {
	return WithRequestCompression(compressionGzip)
}

// WithRequestCompression configures the client to use the specified algorithm to
// compress request messages. If the algorithm has not been registered using
// WithCompression, the generated client constructor will return an error.
//
// Because some servers don't support compression, clients default to sending
// uncompressed requests.
func WithRequestCompression(name string) ClientOption {
	return &requestCompressionOption{Name: name}
}

// A HandlerOption configures a Handler.
//
// In addition to any options grouped in the documentation below, remember that
// all Options are also HandlerOptions.
type HandlerOption interface {
	applyToHandler(*handlerConfiguration)
}

// WithHandlerOptions composes multiple HandlerOptions into one.
func WithHandlerOptions(options ...HandlerOption) HandlerOption {
	return &handlerOptionsOption{options}
}

// Option implements both ClientOption and HandlerOption, so it can be applied
// both client-side and server-side.
type Option interface {
	ClientOption
	HandlerOption
}

// WithCodec registers a serialization method with a client or handler.
// Registering a codec with an empty name is a no-op.
//
// Typically, generated code automatically supplies this option with the
// appropriate codec(s). For example, handlers generated from Protobuf schemas
// using protoc-gen-connect-go automatically register binary and JSON codecs.
// Users with more specialized needs may override the default codecs by
// registering a new codec under the same name.
//
// Handlers may have multiple codecs registered, and use whichever the client
// chooses. Clients may only have a single codec.
func WithCodec(codec Codec) Option {
	return &codecOption{Codec: codec}
}

// WithCompression configures client and server compression strategies. The
// Compressors and Decompressors produced by the supplied constructors must use
// the same algorithm.
//
// For handlers, WithCompression registers a compression algorithm. Clients may
// send messages compressed with that algorithm and/or request compressed
// responses.
//
// For clients, WithCompression serves two purposes. First, the client
// asks servers to compress responses using any of the registered algorithms.
// (gRPC's compression negotiation is complex, but most of Google's gRPC server
// implementations won't compress responses unless the request is compressed.)
// Second, it makes all the registered algorithms available for use with
// WithRequestCompression. Note that actually compressing requests requires
// using both WithCompression and WithRequestCompression.
//
// Calling WithCompression with an empty name or nil constructors is a no-op.
func WithCompression[D Decompressor, C Compressor](
	name string,
	newDecompressor func() D,
	newCompressor func() C,
) Option {
	return &compressionOption{
		Name:            name,
		CompressionPool: newCompressionPool(newDecompressor, newCompressor),
	}
}

// WithCompressMinBytes sets a minimum size threshold for compression:
// regardless of compressor configuration, messages smaller than the configured
// minimum are sent uncompressed.
//
// The default minimum is zero. Setting a minimum compression threshold may
// improve overall performance, because the CPU cost of compressing very small
// messages usually isn't worth the small reduction in network I/O.
func WithCompressMinBytes(min int) Option {
	return &compressMinBytesOption{Min: min}
}

// WithGzip registers a gzip compressor backed by the standard library's gzip
// package with the default compression level.
//
// Handlers with this option applied accept gzipped requests and can send
// gzipped responses. Clients with this option applied request gzipped
// responses, but don't automatically send gzipped requests (since the server
// may not support them). Use WithGzipRequests to gzip requests.
//
// Handlers and clients generated by protoc-gen-connect-go apply WithGzip by
// default.
func WithGzip() Option {
	return WithCompression(
		compressionGzip,
		func() *gzip.Reader { return &gzip.Reader{} },
		func() *gzip.Writer { return gzip.NewWriter(ioutil.Discard) },
	)
}

// WithInterceptors configures a client or handler's interceptor stack. Repeated
// WithInterceptors options are applied in order, so
//
//   WithInterceptors(A) + WithInterceptors(B, C) == WithInterceptors(A, B, C)
//
// Unary interceptors compose like an onion. The first interceptor provided is
// the outermost layer of the onion: it acts first on the context and request,
// and last on the response and error.
//
// Stream interceptors also behave like an onion: the first interceptor
// provided is the first to wrap the context and is the outermost wrapper for
// the (Sender, Receiver) pair. It's the first to see sent messages and the
// last to see received messages.
//
// Applied to client and handler, WithInterceptors(A, B, ..., Y, Z) produces:
//
//        client.Send()     client.Receive()
//              |                 ^
//              v                 |
//           A ---               --- A
//           B ---               --- B
//             ...               ...
//           Y ---               --- Y
//           Z ---               --- Z
//              |                 ^
//              v                 |
//           network            network
//              |                 ^
//              v                 |
//           A ---               --- A
//           B ---               --- B
//             ...               ...
//           Y ---               --- Y
//           Z ---               --- Z
//              |                 ^
//              v                 |
//       handler.Receive() handler.Send()
//              |                 ^
//              |                 |
//              -> handler logic --
//
// Note that in clients, the Sender handles the request message(s) and the
// Receiver handles the response message(s). For handlers, it's the reverse.
// Depending on your interceptor's logic, you may need to wrap one side of the
// stream on the clients and the other side on handlers.
func WithInterceptors(interceptors ...Interceptor) Option {
	return &interceptorsOption{interceptors}
}

// WithOptions composes multiple Options into one.
func WithOptions(options ...Option) Option {
	return &optionsOption{options}
}

// WithProtoBinaryCodec registers a binary Protocol Buffers codec that uses
// google.golang.org/protobuf/proto.
//
// Handlers and clients generated by protoc-gen-connect-go have
// WithProtoBinaryCodec applied by default. To replace the default binary Protobuf
// codec (with vtprotobuf, for example), apply WithCodec with a Codec whose
// name is "proto".
func WithProtoBinaryCodec() Option {
	return WithCodec(&protoBinaryCodec{})
}

// WithProtoJSONCodec registers a codec that serializes Protocol Buffers
// messages as JSON. It uses the standard Protobuf JSON mapping as implemented
// by google.golang.org/protobuf/encoding/protojson: fields are named using
// lowerCamelCase, zero values are omitted, missing required fields are errors,
// enums are emitted as strings, etc.
//
// Handlers generated by protoc-gen-connect-go have WithProtoJSONCodec
// applied by default.
func WithProtoJSONCodec() Option {
	return WithCodec(&protoJSONCodec{})
}

// WithWarn sets the function used to log non-critical internal errors. These
// errors don't impact the server's ability to respond to clients, but do
// indicate a potential problem. For example, Connect handlers use the supplied
// function to log any errors encountered when closing HTTP response bodies.
//
// A typical warn function might log the error, send it to an exception
// reporting system like Sentry, collect metrics, or all of the above. If no
// warn function is supplied, Connect uses the standard library's log.Printf.
//
// Warn functions must be safe to call concurrently.
func WithWarn(warn func(error)) Option {
	return &warnOption{warn}
}

type clientOptionsOption struct {
	options []ClientOption
}

func (o *clientOptionsOption) applyToClient(config *clientConfiguration) {
	for _, option := range o.options {
		option.applyToClient(config)
	}
}

type codecOption struct {
	Codec Codec
}

func (o *codecOption) applyToClient(config *clientConfiguration) {
	if o.Codec == nil || o.Codec.Name() == "" {
		return
	}
	config.Codec = o.Codec
}

func (o *codecOption) applyToHandler(config *handlerConfiguration) {
	if o.Codec == nil || o.Codec.Name() == "" {
		return
	}
	config.Codecs[o.Codec.Name()] = o.Codec
}

type compressionOption struct {
	Name            string
	CompressionPool compressionPool
}

func (o *compressionOption) applyToClient(config *clientConfiguration) {
	o.apply(config.CompressionPools)
}

func (o *compressionOption) applyToHandler(config *handlerConfiguration) {
	o.apply(config.CompressionPools)
}

func (o *compressionOption) apply(m map[string]compressionPool) {
	if o.Name == "" || o.CompressionPool == nil {
		return
	}
	m[o.Name] = o.CompressionPool
}

type compressMinBytesOption struct {
	Min int
}

func (o *compressMinBytesOption) applyToClient(config *clientConfiguration) {
	config.CompressMinBytes = o.Min
}

func (o *compressMinBytesOption) applyToHandler(config *handlerConfiguration) {
	config.CompressMinBytes = o.Min
}

type handlerOptionsOption struct {
	options []HandlerOption
}

func (o *handlerOptionsOption) applyToHandler(config *handlerConfiguration) {
	for _, option := range o.options {
		option.applyToHandler(config)
	}
}

type grpcOption struct {
	web bool
}

func (o *grpcOption) applyToClient(config *clientConfiguration) {
	config.Protocol = &protocolGRPC{web: o.web}
}

type interceptorsOption struct {
	Interceptors []Interceptor
}

func (o *interceptorsOption) applyToClient(config *clientConfiguration) {
	config.Interceptor = o.chainWith(config.Interceptor)
}

func (o *interceptorsOption) applyToHandler(config *handlerConfiguration) {
	config.Interceptor = o.chainWith(config.Interceptor)
}

func (o *interceptorsOption) chainWith(current Interceptor) Interceptor {
	if len(o.Interceptors) == 0 {
		return current
	}
	if current == nil && len(o.Interceptors) == 1 {
		return o.Interceptors[0]
	}
	if current == nil && len(o.Interceptors) > 1 {
		return newChain(o.Interceptors)
	}
	return newChain(append([]Interceptor{current}, o.Interceptors...))
}

type optionsOption struct {
	options []Option
}

func (o *optionsOption) applyToClient(config *clientConfiguration) {
	for _, option := range o.options {
		option.applyToClient(config)
	}
}

func (o *optionsOption) applyToHandler(config *handlerConfiguration) {
	for _, option := range o.options {
		option.applyToHandler(config)
	}
}

type requestCompressionOption struct {
	Name string
}

func (o *requestCompressionOption) applyToClient(config *clientConfiguration) {
	config.RequestCompressionName = o.Name
}

type warnOption struct {
	Warn func(error)
}

func (o *warnOption) applyToClient(config *clientConfiguration) {
	config.Warn = o.Warn
}

func (o *warnOption) applyToHandler(config *handlerConfiguration) {
	config.Warn = o.Warn
}
