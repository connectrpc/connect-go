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
	"context"
	"net/http"

	"connectrpc.com/connect/v2"
)

// streamingHandlerFunc is the signature of a streaming RPC from the handler's
// perspective.
type streamingHandlerFunc func(context.Context, streamingHandlerConn, *connect.CallInfo) error

// A handler is the server-side implementation of a single RPC defined by a
// service schema.
//
// By default, handlers support the Connect, gRPC, and gRPC-Web protocols with
// the binary Protobuf and JSON codecs. They support gzip compression using the
// standard library's [compress/gzip].
type handler struct {
	spec             connect.Spec
	implementation   streamingHandlerFunc
	protocolHandlers map[string][]protocolHandler // Method to protocol handlers
	allowMethod      string                       // Allow header
	acceptPost       string                       // Accept-Post header
}

// ServeHTTP implements [http.Handler].
func (h *handler) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	// We don't need to defer functions to close the request body or read to
	// EOF: the stream we construct later on already does that, and we only
	// return early when dealing with misbehaving clients. In those cases, it's
	// okay if we can't re-use the connection.
	isBidi := (h.spec.StreamType & connect.StreamTypeBidi) == connect.StreamTypeBidi
	if isBidi && request.ProtoMajor < 2 {
		// Clients coded to expect full-duplex connections may hang if they've
		// mistakenly negotiated HTTP/1.1. To unblock them, we must close the
		// underlying TCP connection.
		responseWriter.Header().Set("Connection", "close")
		responseWriter.WriteHeader(http.StatusHTTPVersionNotSupported)
		return
	}

	protocolHandlers := h.protocolHandlers[request.Method]
	if len(protocolHandlers) == 0 {
		responseWriter.Header().Set("Allow", h.allowMethod)
		responseWriter.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	contentType := canonicalizeContentType(getHeaderCanonical(request.Header, headerContentType))

	// Find our implementation of the RPC protocol in use.
	var protocolHandler protocolHandler
	for _, handler := range protocolHandlers {
		if handler.CanHandlePayload(request, contentType) {
			protocolHandler = handler
			break
		}
	}
	if protocolHandler == nil {
		responseWriter.Header().Set("Accept-Post", h.acceptPost)
		responseWriter.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}

	if request.Method == http.MethodGet {
		// A body must not be present.
		hasBody := request.ContentLength > 0
		if request.ContentLength < 0 {
			// No content-length header.
			// Test if body is empty by trying to read a single byte.
			var b [1]byte
			n, _ := request.Body.Read(b[:])
			hasBody = n > 0
		}
		if hasBody {
			responseWriter.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		_ = request.Body.Close()
	}

	// Establish a stream and serve the RPC.
	setHeaderCanonical(request.Header, headerContentType, contentType)
	setHeaderCanonical(request.Header, headerHost, request.Host)
	ctx, cancel, timeoutErr := protocolHandler.SetTimeout(request) //nolint: contextcheck
	if timeoutErr != nil {
		ctx = request.Context()
	}
	if cancel != nil {
		defer cancel()
	}
	info := &connect.CallInfo{TransportInfo: &ServerInfo{request: request}}
	connCloser, ok := protocolHandler.NewConn(
		responseWriter,
		request.WithContext(ctx),
		info,
	)
	if !ok {
		// Failed to create stream, usually because client used an unknown
		// compression algorithm. Nothing further to do.
		return
	}
	if timeoutErr != nil {
		_ = connCloser.Close(timeoutErr)
		return
	}
	_ = connCloser.Close(h.implementation(ctx, connCloser, info))
}

type handlerConfig struct {
	CompressionPools             map[string]*compressionPool
	CompressionNames             []string
	Codecs                       map[string]connect.Codec
	CompressMinBytes             int
	Procedure                    string
	Schema                       any
	RequireConnectProtocolHeader bool
	IdempotencyLevel             connect.IdempotencyLevel
	ReadMaxBytes                 int
	SendMaxBytes                 int
	StreamType                   connect.StreamType
}

func (c *handlerConfig) newSpec() connect.Spec {
	return connect.Spec{
		StreamType:       c.StreamType,
		IdempotencyLevel: c.IdempotencyLevel,
		Schema:           c.Schema,
		Procedure:        c.Procedure,
	}
}

func (c *handlerConfig) newProtocolHandlers() []protocolHandler {
	protocols := []protocol{
		&protocolConnect{},
		&protocolGRPC{web: false},
		&protocolGRPC{web: true},
	}
	handlers := make([]protocolHandler, 0, len(protocols))
	codecs := newReadOnlyCodecs(c.Codecs)
	compressors := newReadOnlyCompressionPools(
		c.CompressionPools,
		c.CompressionNames,
	)
	for _, protocol := range protocols {
		handlers = append(handlers, protocol.NewHandler(&protocolHandlerParams{
			spec:                         c.newSpec(),
			Codecs:                       codecs,
			CompressionPools:             compressors,
			CompressMinBytes:             c.CompressMinBytes,
			ReadMaxBytes:                 c.ReadMaxBytes,
			SendMaxBytes:                 c.SendMaxBytes,
			RequireConnectProtocolHeader: c.RequireConnectProtocolHeader,
			IdempotencyLevel:             c.IdempotencyLevel,
		}))
	}
	return handlers
}
