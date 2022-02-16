package connect

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bufconnect/connect/codec"
	"github.com/bufconnect/connect/compress"
)

type grpc struct {
	web bool
}

func (g *grpc) NewHandler(params *protocolHandlerParams) (protocolHandler, error) {
	return &grpcHandler{
		spec:            params.Spec,
		web:             g.web,
		codecs:          params.Codecs,
		compressors:     params.Compressors,
		maxRequestBytes: params.MaxRequestBytes,
		accept:          acceptPostValue(g.web, params.Codecs),
	}, nil
}

func (g *grpc) NewClient(params *protocolClientParams) (protocolClient, error) {
	procedureURL := params.BaseURL + "/" + params.Spec.Procedure
	if _, err := url.ParseRequestURI(procedureURL); err != nil {
		return nil, Wrap(CodeUnknown, err)
	}
	return &grpcClient{
		spec:             params.Spec,
		web:              g.web,
		compressorName:   params.CompressorName,
		compressors:      params.Compressors,
		codecName:        params.CodecName,
		codec:            params.Codec,
		protobuf:         params.Protobuf,
		maxResponseBytes: params.MaxResponseBytes,
		doer:             params.Doer,
		procedureURL:     procedureURL,
	}, nil
}

type grpcHandler struct {
	spec            Specification
	web             bool
	codecs          readOnlyCodecs
	compressors     roCompressors
	maxRequestBytes int64
	accept          string
}

func (g *grpcHandler) ShouldHandleMethod(method string) bool {
	return method == http.MethodPost
}

func (g *grpcHandler) ShouldHandleContentType(ctype string) bool {
	codecName := codecFromContentType(g.web, ctype)
	if codecName == "" {
		return false // not a gRPC content-type
	}
	return g.codecs.Get(codecName) != nil
}

func (g *grpcHandler) WriteAccept(h http.Header) {
	if prev := h.Get("Allow"); prev != "" {
		h.Set("Allow", prev+", "+http.MethodPost)
	} else {
		h.Set("Allow", http.MethodPost)
	}
	if prev := h.Get("Accept-Post"); prev != "" {
		h.Set("Accept-Post", prev+", "+g.accept)
	} else {
		h.Set("Accept-Post", g.accept)
	}
}

func (g *grpcHandler) NewStream(w http.ResponseWriter, r *http.Request) (Sender, Receiver, error) {
	codecName := codecFromContentType(g.web, r.Header.Get("Content-Type"))
	// ShouldHandleContentType guarantees that this is non-nil
	clientCodec := g.codecs.Get(codecName)

	// We need to parse metadata before entering the interceptor stack, but we'd
	// like to report errors to the client in a format they understand (if
	// possible). We'll collect any such errors here; once we return, the Handler
	// will send the error to the client.
	var failed *Error

	timeout, err := parseTimeout(r.Header.Get("Grpc-Timeout"))
	if err != nil && err != errNoTimeout {
		// Errors here indicate that the client sent an invalid timeout header, so
		// the error text is safe to send back.
		failed = Wrap(CodeInvalidArgument, err)
	} else if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		r = r.WithContext(ctx)
	} // else err == errNoTimeout, nothing to do

	requestCompression := compress.NameIdentity
	if me := r.Header.Get("Grpc-Encoding"); me != "" && me != compress.NameIdentity {
		// We default to identity, so we only care if the client sends something
		// other than the empty string or compress.NameIdentity.
		if g.compressors.Contains(me) {
			requestCompression = me
		} else if failed == nil {
			// Per https://github.com/grpc/grpc/blob/master/doc/compression.md, we
			// should return CodeUnimplemented and specify acceptable compression(s)
			// (in addition to setting the Grpc-Accept-Encoding header).
			failed = Errorf(
				CodeUnimplemented,
				"unknown compression %q: accepted grpc-encoding values are %v",
				me, g.compressors.Names(),
			)
		}
	}
	// Follow https://github.com/grpc/grpc/blob/master/doc/compression.md.
	// (The grpc-go implementation doesn't read the "grpc-accept-encoding" header
	// and doesn't support compression method asymmetry.)
	responseCompression := requestCompression
	// If we're not already planning to compress the response, check whether the
	// client requested a compression algorithm we support.
	if responseCompression == compress.NameIdentity {
		if mae := r.Header.Get("Grpc-Accept-Encoding"); mae != "" {
			for _, enc := range strings.FieldsFunc(mae, isCommaOrSpace) {
				if g.compressors.Contains(enc) {
					// We found a mutually supported compression algorithm. Unlike standard
					// HTTP, there's no preference weighting, so can bail out immediately.
					responseCompression = enc
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
	w.Header()["Content-Type"] = []string{r.Header.Get("Content-Type")}
	w.Header()["Grpc-Accept-Encoding"] = []string{g.compressors.Names()}
	w.Header()["Grpc-Encoding"] = []string{responseCompression}
	if !g.web {
		// Every standard gRPC response will have these trailers, but gRPC-Web
		// doesn't use HTTP trailers.
		w.Header()["Trailer"] = []string{"Grpc-Status", "Grpc-Message", "Grpc-Status-Details-Bin"}
	}

	sender, receiver := g.wrapStream(newHandlerStream(
		g.spec,
		g.web,
		w,
		r,
		g.maxRequestBytes,
		clientCodec,
		g.codecs.Protobuf(), // for errors
		g.compressors.Get(requestCompression),
		g.compressors.Get(responseCompression),
	))
	// We can't return failed as-is: a nil *Error is non-nil when returned as an
	// error interface.
	if failed != nil {
		return sender, receiver, failed
	}
	return sender, receiver, nil
}

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
	spec                 Specification
	web                  bool
	compressorName       string
	compressors          roCompressors
	codecName            string
	codec                codec.Codec
	protobuf             codec.Codec
	maxResponseBytes     int64
	doer                 Doer
	procedureURL         string
	wrapErrorInterceptor Interceptor
}

func (g *grpcClient) WriteRequestHeader(h http.Header) {
	// We know these header keys are in canonical form, so we can bypass all the
	// checks in Header.Set.
	h["User-Agent"] = userAgent
	h["Content-Type"] = []string{contentTypeFromCodecName(g.web, g.codecName)}
	if g.compressorName != "" && g.compressorName != compress.NameIdentity {
		h["Grpc-Encoding"] = []string{g.compressorName}
	}
	if acceptCompression := g.compressors.Names(); acceptCompression != "" {
		h["Grpc-Accept-Encoding"] = []string{acceptCompression}
	}
	if !g.web {
		// No HTTP trailers in gRPC-Web.
		h["Te"] = []string{"trailers"}
	}
}

func (g *grpcClient) NewStream(ctx context.Context, h http.Header) (Sender, Receiver) {
	// In a typical HTTP/1.1 request, we'd put the body into a bytes.Buffer, hand
	// the buffer to http.NewRequest, and fire off the request with doer.Do. That
	// won't work here because we're establishing a stream - we don't even have
	// all the data we'll eventually send. Instead, we use io.Pipe as the request
	// body.
	//
	// net/http will own the read side of the pipe, and we'll hold onto the write
	// side. Writes to pw will block until net/http pulls the data from pr and
	// puts it onto the network - there's no buffer between the two. (The two
	// sides of the pipe are meant to be used concurrently.) Once the server gets
	// the first protobuf message that we send, it'll send back headers and start
	// the response stream.
	pr, pw := io.Pipe()
	duplex := &clientStream{
		ctx:          ctx,
		doer:         g.doer,
		url:          g.procedureURL,
		spec:         g.spec,
		maxReadBytes: g.maxResponseBytes,
		codec:        g.codec,
		protobuf:     g.protobuf,
		writer:       pw,
		marshaler: marshaler{
			w:          pw,
			compressor: g.compressors.Get(g.compressorName),
			codec:      g.codec,
		},
		header:        h,
		web:           g.web,
		reader:        pr,
		compressors:   g.compressors,
		responseReady: make(chan struct{}),
	}
	return g.wrapStream(&clientSender{duplex}, &clientReceiver{duplex})
}

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
