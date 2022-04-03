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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type protocolGRPC struct {
	web bool
}

// NewHandler implements protocol, so it must return an interface.
func (g *protocolGRPC) NewHandler(params *protocolHandlerParams) protocolHandler {
	return &grpcHandler{
		spec:             params.Spec,
		web:              g.web,
		codecs:           params.Codecs,
		compressionPools: params.CompressionPools,
		maxRequestBytes:  params.MaxRequestBytes,
		minCompressBytes: params.CompressMinBytes,
		accept:           acceptPostValue(g.web, params.Codecs),
	}
}

// NewClient implements protocol, so it must return an interface.
func (g *protocolGRPC) NewClient(params *protocolClientParams) (protocolClient, error) {
	if _, err := url.ParseRequestURI(params.URL); err != nil {
		if !strings.Contains(params.URL, "://") {
			// URL doesn't have a scheme, so the user is likely accustomed to
			// grpc-go's APIs.
			err = fmt.Errorf("URL %q missing scheme: use http:// or https:// (unlike grpc-go)", params.URL)
		}
		return nil, NewError(CodeUnknown, err)
	}
	return &grpcClient{
		web:              g.web,
		compressionName:  params.CompressionName,
		compressionPools: params.CompressionPools,
		codec:            params.Codec,
		protobuf:         params.Protobuf,
		maxResponseBytes: params.MaxResponseBytes,
		minCompressBytes: params.CompressMinBytes,
		httpClient:       params.HTTPClient,
		procedureURL:     params.URL,
	}, nil
}

type grpcHandler struct {
	spec             Specification
	web              bool
	codecs           readOnlyCodecs
	compressionPools readOnlyCompressionPools
	maxRequestBytes  int64
	minCompressBytes int
	accept           string
}

func (g *grpcHandler) ShouldHandleMethod(method string) bool {
	return method == http.MethodPost
}

func (g *grpcHandler) ShouldHandleContentType(contentType string) bool {
	codecName := codecFromContentType(g.web, contentType)
	if codecName == "" {
		return false // not a gRPC content-type
	}
	return g.codecs.Get(codecName) != nil
}

func (g *grpcHandler) WriteAccept(header http.Header) {
	addCommaSeparatedHeader(header, "Allow", http.MethodPost)
	addCommaSeparatedHeader(header, "Accept-Post", g.accept)
}

func (g *grpcHandler) NewStream(
	responseWriter http.ResponseWriter,
	request *http.Request,
) (Sender, Receiver, error) {
	codecName := codecFromContentType(g.web, request.Header.Get("Content-Type"))
	// ShouldHandleContentType guarantees that this is non-nil
	clientCodec := g.codecs.Get(codecName)

	// We need to parse metadata before entering the interceptor stack, but we'd
	// like to report errors to the client in a format they understand (if
	// possible). We'll collect any such errors here; once we return, the Handler
	// will send the error to the client.
	var failed *Error

	timeout, err := parseTimeout(request.Header.Get("Grpc-Timeout"))
	if err != nil && !errors.Is(err, errNoTimeout) {
		// Errors here indicate that the client sent an invalid timeout header, so
		// the error text is safe to send back.
		failed = NewError(CodeInvalidArgument, err)
	} else if err == nil {
		ctx, cancel := context.WithTimeout(request.Context(), timeout)
		defer cancel()
		request = request.WithContext(ctx)
	} // else err wraps errNoTimeout, nothing to do

	requestCompression := compressionIdentity
	if msgEncoding := request.Header.Get("Grpc-Encoding"); msgEncoding != "" && msgEncoding != compressionIdentity {
		// We default to identity, so we only care if the client sends something
		// other than the empty string or compressIdentity.
		if g.compressionPools.Contains(msgEncoding) {
			requestCompression = msgEncoding
		} else if failed == nil {
			// Per https://github.com/grpc/grpc/blob/master/doc/compression.md, we
			// should return CodeUnimplemented and specify acceptable compression(s)
			// (in addition to setting the Grpc-Accept-Encoding header).
			failed = errorf(
				CodeUnimplemented,
				"unknown compression %q: accepted grpc-encoding values are %v",
				msgEncoding, g.compressionPools.CommaSeparatedNames(),
			)
		}
	}
	// Support asymmetric compression, following
	// https://github.com/grpc/grpc/blob/master/doc/compression.md. (The grpc-go
	// implementation doesn't read the "grpc-accept-encoding" header and doesn't
	// support asymmetry.)
	responseCompression := requestCompression
	// If we're not already planning to compress the response, check whether the
	// client requested a compression algorithm we support.
	if responseCompression == compressionIdentity {
		if acceptEncoding := request.Header.Get("Grpc-Accept-Encoding"); acceptEncoding != "" {
			for _, name := range strings.FieldsFunc(acceptEncoding, isCommaOrSpace) {
				if g.compressionPools.Contains(name) {
					// We found a mutually supported compression algorithm. Unlike standard
					// HTTP, there's no preference weighting, so can bail out immediately.
					responseCompression = name
					break
				}
			}
		}
	}

	// We must write any remaining headers here:
	// (1) any writes to the stream will implicitly send the headers, so we
	// should get all of gRPC's required response headers ready.
	// (2) interceptors should be able to see these headers.
	//
	// Since we know that these header keys are already in canonical form, we can
	// skip the normalization in Header.Set.
	responseWriter.Header()["Content-Type"] = []string{request.Header.Get("Content-Type")}
	responseWriter.Header()["Grpc-Accept-Encoding"] = []string{g.compressionPools.CommaSeparatedNames()}
	if responseCompression != compressionIdentity {
		responseWriter.Header()["Grpc-Encoding"] = []string{responseCompression}
	}

	sender, receiver := g.wrapStream(newHandlerStream(
		g.spec,
		g.web,
		responseWriter,
		request,
		g.maxRequestBytes,
		g.minCompressBytes,
		clientCodec,
		g.codecs.Protobuf(), // for errors
		g.compressionPools.Get(requestCompression),
		g.compressionPools.Get(responseCompression),
	))
	// We can't return failed as-is: a nil *Error is non-nil when returned as an
	// error interface.
	if failed != nil {
		// Negotiation failed, so we can't establish a stream. To make the
		// request's HTTP trailers visible to interceptors, we should try to read
		// the body to EOF.
		discard(request.Body)
		return sender, receiver, failed
	}
	return sender, receiver, nil
}

// wrapStream ensures that we (1) automatically code context-related errors
// correctly when writing them to the network, and (2) return *Errors from all
// exported APIs.
func (g *grpcHandler) wrapStream(sender Sender, receiver Receiver) (Sender, Receiver) {
	wrappedSender := &errorTranslatingSender{
		Sender:   sender,
		toWire:   wrapIfContextError,
		fromWire: wrapIfUncoded,
	}
	wrappedReceiver := &errorTranslatingReceiver{
		Receiver: receiver,
		fromWire: wrapIfUncoded,
	}
	return wrappedSender, wrappedReceiver
}

type grpcClient struct {
	web                  bool
	compressionName      string
	compressionPools     readOnlyCompressionPools
	codec                Codec
	protobuf             Codec
	maxResponseBytes     int64
	minCompressBytes     int
	httpClient           HTTPClient
	procedureURL         string
	wrapErrorInterceptor Interceptor
}

func (g *grpcClient) NewStream(
	ctx context.Context,
	spec Specification,
	header http.Header,
) (Sender, Receiver) {
	// We know these header keys are in canonical form, so we can bypass all the
	// checks in Header.Set.
	header["User-Agent"] = []string{userAgent()}
	header["Content-Type"] = []string{contentTypeFromCodecName(g.web, g.codec.Name())}
	if g.compressionName != "" && g.compressionName != compressionIdentity {
		header["Grpc-Encoding"] = []string{g.compressionName}
	}
	if acceptCompression := g.compressionPools.CommaSeparatedNames(); acceptCompression != "" {
		header["Grpc-Accept-Encoding"] = []string{acceptCompression}
	}
	if !g.web {
		// No HTTP trailers in gRPC-Web.
		header["Te"] = []string{"trailers"}
	}
	// In a typical HTTP/1.1 request, we'd put the body into a bytes.Buffer, hand
	// the buffer to http.NewRequest, and fire off the request with
	// httpClient.Do. That won't work here because we're establishing a stream -
	// we don't even have all the data we'll eventually send. Instead, we use
	// io.Pipe as the request body.
	//
	// net/http will own the read side of the pipe, and we'll hold onto the write
	// side. Writes to pipeWriter will block until net/http pulls the data from pipeReader and
	// puts it onto the network - there's no buffer between the two. (The two
	// sides of the pipe are meant to be used concurrently.) Once the server gets
	// the first Protobuf message that we send, it'll send back headers and start
	// the response stream.
	pipeReader, pipeWriter := io.Pipe()
	duplex := &duplexClientStream{
		ctx:          ctx,
		httpClient:   g.httpClient,
		url:          g.procedureURL,
		spec:         spec,
		maxReadBytes: g.maxResponseBytes,
		codec:        g.codec,
		protobuf:     g.protobuf,
		writer:       pipeWriter,
		marshaler: marshaler{
			writer:           pipeWriter,
			compressionPool:  g.compressionPools.Get(g.compressionName),
			codec:            g.codec,
			compressMinBytes: g.minCompressBytes,
		},
		header:           header,
		trailer:          make(http.Header),
		web:              g.web,
		reader:           pipeReader,
		responseHeader:   make(http.Header),
		responseTrailer:  make(http.Header),
		compressionPools: g.compressionPools,
		responseReady:    make(chan struct{}),
	}
	return g.wrapStream(&clientSender{duplex}, &clientReceiver{duplex})
}

// wrapStream ensures that we always return *Errors from public APIs.
func (g *grpcClient) wrapStream(sender Sender, receiver Receiver) (Sender, Receiver) {
	wrappedSender := &errorTranslatingSender{
		Sender:   sender,
		toWire:   func(err error) error { return err }, // no-op
		fromWire: wrapIfUncoded,
	}
	wrappedReceiver := &errorTranslatingReceiver{
		Receiver: receiver,
		fromWire: wrapIfUncoded,
	}
	return wrappedSender, wrappedReceiver
}
