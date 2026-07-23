// Copyright 2021-2026 The Connect Authors
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

package connecthttp

import (
	"slices"

	"connectrpc.com/connect/v2"
)

// Option configures [NewTransport], [Mount], and [NewErrorWriter]. A single
// Option list builds up one [options]; each entry point reads the settings that
// apply to it and ignores the rest. Where an option only affects clients or
// only affects servers, the documentation says so.
type Option interface {
	apply(*options)
}

// WithGRPC configures clients to use the HTTP/2 gRPC protocol. The default is
// the Connect protocol. [Mount] ignores this option; handlers accept all three
// protocols by content-type negotiation.
func WithGRPC() Option {
	return protocolOption{name: connect.ProtocolNameGRPC}
}

// WithGRPCWeb configures clients to use the gRPC-Web protocol. The default is
// the Connect protocol. [Mount] ignores this option; handlers accept all three
// protocols by content-type negotiation.
func WithGRPCWeb() Option {
	return protocolOption{name: connect.ProtocolNameGRPCWeb}
}

// WithProtoJSON configures a client to send JSON-encoded data instead of
// binary Protobuf. It uses the standard Protobuf JSON mapping as implemented by
// [google.golang.org/protobuf/encoding/protojson]: fields are named using
// lowerCamelCase, zero values are omitted, missing required fields are errors,
// enums are emitted as strings, etc.
func WithProtoJSON() Option {
	return WithSendCodec(connect.CodecNameJSON)
}

// WithSendCodec selects the codec (by name) used for outgoing client requests.
// The codec must also be registered via [WithCodec]. Defaults to
// [connect.CodecNameProto]. [Mount] ignores this option.
func WithSendCodec(name string) Option {
	return sendCodecOption(name)
}

// WithCodec registers a [connect.Codec]. Clients may use it for outgoing
// requests. Servers accept it on inbound requests. Multiple WithCodec options
// merge. Defaults to
// [connectrpc.com/connect/v2/connectproto.BinaryCodec] and
// [connectrpc.com/connect/v2/connectproto.JSONCodec].
func WithCodec(codec connect.Codec) Option {
	return codecsOption([]connect.Codec{codec})
}

// WithCompressor registers a [connect.Compressor]. Clients advertise it in
// Accept-Encoding and may use it for sending; servers accept it on inbound
// requests and use it for responses. Compressors add to the default set, and
// the most recently registered is the most preferred. Multiple WithCompressor
// options accumulate.
//
// The default is gzip. Use [WithNoCompression] to disable compression.
func WithCompressor(compressor connect.Compressor) Option {
	return compressorsOption([]connect.Compressor{compressor})
}

// WithNoCompression disables compression on both clients and servers: it
// removes every registered compressor (including the default gzip) so nothing
// is advertised, accepted, or sent compressed. A later [WithCompressor]
// re-registers compressors after it.
func WithNoCompression() Option {
	return noCompressionOption{}
}

// WithAcceptCompression makes a compression algorithm available to a client or
// handler by name, advertising it in Accept-Encoding. The named compressor must
// be registered via [WithCompressor]; the name simply selects from that pool.
// The first registered algorithm is treated as the least preferred, and the
// last registered algorithm is the most preferred.
//
// It's safe to use this option liberally: servers will ignore any compression
// algorithms they don't support. To compress requests, pair this option with
// [WithSendCompression].
//
// Clients and servers support gzip by default.
//
// Calling WithAcceptCompression with an empty name is a no-op.
func WithAcceptCompression(name string) Option {
	return acceptCompressionOption(name)
}

// WithSendCompression selects the compressor (by name) to apply to outgoing
// client payloads. The compressor must be registered via [WithCompressor] (or
// be the default "gzip" compressor). Requests are sent uncompressed by default,
// to support servers that don't support compression. [Mount] ignores this
// option.
func WithSendCompression(name string) Option {
	return sendCompressorOption(name)
}

// WithSendGzip configures the client to gzip requests. Since clients have
// access to a gzip compressor by default, WithSendGzip doesn't require
// [WithCompressor].
//
// Some servers don't support gzip, so clients default to sending uncompressed
// requests.
func WithSendGzip() Option {
	return WithSendCompression(connect.CompressionNameGzip)
}

// WithCompressMinBytes sets a minimum size threshold for compression:
// regardless of compressor configuration, messages smaller than the configured
// minimum are sent uncompressed.
//
// The default minimum is zero. Setting a minimum compression threshold may
// improve overall performance, because the CPU cost of compressing very small
// messages usually isn't worth the small reduction in network I/O.
func WithCompressMinBytes(n int) Option {
	return compressMinBytesOption(n)
}

// WithReadMaxBytes limits the performance impact of pathologically large
// messages sent by the other party. For handlers, WithReadMaxBytes limits the
// size of a message that the client can send. For clients, WithReadMaxBytes
// limits the size of a message that the server can respond with. Limits apply
// to each Protobuf message, not to the stream as a whole.
//
// Setting WithReadMaxBytes to zero allows any message size. Both clients and
// handlers default to allowing any request size.
//
// Handlers may also use [net/http.MaxBytesHandler] to limit the total size of
// the HTTP request stream (rather than the per-message size). Connect handles
// [net/http.MaxBytesError] specially, so clients still receive errors with the
// appropriate error code and informative messages.
func WithReadMaxBytes(n int) Option {
	return readMaxBytesOption(n)
}

// WithSendMaxBytes prevents sending messages too large for the client/handler
// to handle without significant performance overhead. For handlers,
// WithSendMaxBytes limits the size of a message that the handler can respond
// with. For clients, WithSendMaxBytes limits the size of a message that the
// client can send. Limits apply to each message, not to the stream as a whole.
//
// Setting WithSendMaxBytes to zero allows any message size. Both clients and
// handlers default to allowing any message size.
func WithSendMaxBytes(n int) Option {
	return sendMaxBytesOption(n)
}

// WithRequireConnectProtocolHeader configures the handler to require requests
// using the Connect RPC protocol to include the Connect-Protocol-Version
// header. This ensures that HTTP proxies and net/http middleware can easily
// identify valid Connect requests, even if they use a common Content-Type like
// application/json. However, it makes ad-hoc requests with tools like cURL more
// laborious. Streaming requests are not affected by this option.
//
// This option has no effect if the client uses the gRPC or gRPC-Web protocols.
// [NewTransport] ignores this option.
func WithRequireConnectProtocolHeader() Option {
	return requireConnectProtocolHeaderOption{}
}

// WithHTTPGet allows Connect-protocol clients to use HTTP GET requests for
// side-effect free unary RPC calls. Typically, the service schema indicates
// which procedures are idempotent. The gRPC and gRPC-Web protocols are
// POST-only, so this option has no effect when combined with [WithGRPC] or
// [WithGRPCWeb].
//
// Using HTTP GET requests makes it easier to take advantage of CDNs, caching
// reverse proxies, and browsers' built-in caching. Note, however, that servers
// don't automatically set any cache headers; you can set cache headers using
// interceptors or by adding headers in individual procedure implementations.
//
// By default, all requests are made as HTTP POSTs. [Mount] ignores this option.
func WithHTTPGet() Option {
	return enableGetOption{}
}

// WithHTTPGetMaxURLSize sets the maximum allowable URL length for GET requests
// made using the Connect protocol. It has no effect on gRPC or gRPC-Web
// clients, since those protocols are POST-only.
//
// Limiting the URL size is useful as most user agents, proxies, and servers
// have limits on the allowable length of a URL. For example, Apache and Nginx
// limit the size of a request line to around 8 KiB, meaning that maximum length
// of a URL is a bit smaller than this. If you run into URL size limitations
// imposed by your network infrastructure and don't know the maximum allowable
// size, or if you'd prefer to be cautious from the start, a 4096 byte (4 KiB)
// limit works with most common proxies and CDNs.
//
// If fallback is set to true and the URL would be longer than the configured
// maximum value, the request will be sent as an HTTP POST instead. If fallback
// is set to false, the request will fail with [connect.CodeResourceExhausted].
//
// By default, Connect-protocol clients with GET requests enabled may send a URL
// of any size. [Mount] ignores this option.
func WithHTTPGetMaxURLSize(bytes int, fallback bool) Option {
	return getURLMaxBytesOption{max: bytes, fallback: fallback}
}

// WithConditionalOptions allows procedures to have different configurations.
// For example, one procedure may need a much larger [WithReadMaxBytes] setting
// than the others.
//
// WithConditionalOptions takes a function which may inspect each procedure's
// [connect.Spec] before deciding which options to apply. Both clients and
// servers evaluate it per procedure. Returning a nil slice is safe.
func WithConditionalOptions(conditional func(spec connect.Spec) []Option) Option {
	return conditionalOption{conditional: conditional}
}

type protocolOption struct{ name string }

// apply sets the client's outgoing protocol. A client speaks exactly one
// protocol, so the last option wins.
func (o protocolOption) apply(opts *options) { opts.protocol = o.name }

type sendCodecOption string

func (o sendCodecOption) apply(opts *options) { opts.sendCodecName = string(o) }

type codecsOption []connect.Codec

func (o codecsOption) apply(opts *options) {
	if opts.codecs == nil {
		opts.codecs = make(map[string]connect.Codec, len(o))
	}
	for _, c := range o {
		opts.codecs[c.Name()] = c
	}
}

type compressorsOption []connect.Compressor

func (o compressorsOption) apply(opts *options) {
	for _, c := range o {
		if _, dup := opts.compressors[c.Name()]; !dup {
			opts.compressorNames = append(opts.compressorNames, c.Name())
		}
		opts.compressors[c.Name()] = c
	}
}

type noCompressionOption struct{}

func (noCompressionOption) apply(opts *options) {
	opts.compressors = map[string]connect.Compressor{}
	opts.compressorNames = nil
}

type acceptCompressionOption string

func (o acceptCompressionOption) apply(opts *options) {
	name := string(o)
	if name == "" {
		return
	}
	if !slices.Contains(opts.compressorNames, name) {
		opts.compressorNames = append(opts.compressorNames, name)
	}
}

type sendCompressorOption string

func (o sendCompressorOption) apply(opts *options) { opts.sendCompressor = string(o) }

type compressMinBytesOption int

func (o compressMinBytesOption) apply(opts *options) { opts.compressMinBytes = int(o) }

type readMaxBytesOption int

func (o readMaxBytesOption) apply(opts *options) { opts.readMaxBytes = int(o) }

type sendMaxBytesOption int

func (o sendMaxBytesOption) apply(opts *options) { opts.sendMaxBytes = int(o) }

type requireConnectProtocolHeaderOption struct{}

func (requireConnectProtocolHeaderOption) apply(opts *options) {
	opts.requireConnectProtocolHeader = true
}

type enableGetOption struct{}

func (enableGetOption) apply(opts *options) { opts.getEnabled = true }

type getURLMaxBytesOption struct {
	max      int
	fallback bool
}

func (o getURLMaxBytesOption) apply(opts *options) {
	opts.getMaxURLBytes = o.max
	opts.getUseFallback = o.fallback
}

type conditionalOption struct {
	conditional func(connect.Spec) []Option
}

func (o conditionalOption) apply(opts *options) {
	if o.conditional == nil {
		return
	}
	opts.conditional = append(opts.conditional, o.conditional)
}
