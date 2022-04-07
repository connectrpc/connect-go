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
	"errors"
	"net/http"
)

// Thankfully, the handler stream is much simpler than the client. net/http
// gives us the request body and response writer at the same time, so we don't
// need to worry about concurrency.
func newHandlerStream(
	spec Specification,
	web bool,
	responseWriter http.ResponseWriter,
	request *http.Request,
	compressMinBytes int,
	codec Codec,
	protobuf Codec, // for errors
	requestCompressionPools compressionPool,
	responseCompressionPools compressionPool,
) (*handlerSender, *handlerReceiver) {
	sender := &handlerSender{
		spec: spec,
		web:  web,
		marshaler: marshaler{
			writer:           responseWriter,
			compressionPool:  responseCompressionPools,
			codec:            codec,
			compressMinBytes: compressMinBytes,
		},
		protobuf: protobuf,
		writer:   responseWriter,
		header:   make(http.Header),
		trailer:  make(http.Header),
	}
	receiver := &handlerReceiver{
		spec: spec,
		unmarshaler: unmarshaler{
			web:             web,
			reader:          request.Body,
			compressionPool: requestCompressionPools,
			codec:           codec,
		},
		request: request,
	}
	return sender, receiver
}

type handlerSender struct {
	spec        Specification
	web         bool
	marshaler   marshaler
	protobuf    Codec // for errors
	writer      http.ResponseWriter
	header      http.Header
	trailer     http.Header
	wroteToBody bool
}

var _ Sender = (*handlerSender)(nil)

func (hs *handlerSender) Send(message any) error {
	defer hs.flush()
	if !hs.wroteToBody {
		mergeHeaders(hs.writer.Header(), hs.header)
		hs.wroteToBody = true
	}
	if err := hs.marshaler.Marshal(message); err != nil {
		return err // already coded
	}
	// don't return typed nils
	return nil
}

func (hs *handlerSender) Close(err error) error {
	defer hs.flush()
	// If we haven't written the headers yet, do so.
	if !hs.wroteToBody {
		mergeHeaders(hs.writer.Header(), hs.header)
	}
	// gRPC always sends the error's code, message, details, and metadata as
	// trailers. Future protocols may not do this, though, so we don't want to
	// mutate the trailers map that the user sees.
	mergedTrailers := make(http.Header, len(hs.trailer)+2) // always make space for status & message
	mergeHeaders(mergedTrailers, hs.trailer)
	if marshalErr := grpcErrorToTrailer(mergedTrailers, hs.protobuf, err); marshalErr != nil {
		return marshalErr
	}
	if hs.web && !hs.wroteToBody {
		// We're using gRPC-Web and we haven't yet written to the body. Since we're
		// not sending any response messages, the gRPC specification calls this a
		// "trailers-only" response. Under those circumstances, the gRPC-Web spec
		// says that implementations _may_ send trailing metadata as HTTP headers
		// instead. The gRPC-Web spec is explicitly a description of the reference
		// implementations rather than a proper specification, so we're going to
		// emulate Envoy and put the trailing metadata in the HTTP headers.
		mergeHeaders(hs.writer.Header(), mergedTrailers)
		return nil
	}
	if hs.web {
		// We're using gRPC-Web and we've already sent the headers, so we write
		// trailing metadata to the HTTP body.
		if err := hs.marshaler.MarshalWebTrailers(mergedTrailers); err != nil {
			return err
		}
		// Don't return typed nils.
		return nil
	}
	// We're using standard gRPC. Even if we haven't written to the body and
	// we're sending a "trailers-only" response, we must send trailing metadata
	// as HTTP trailers. (If we had frame-level control of the HTTP/2 layer, we
	// could send a single HEADER frame and no DATA frames, but net/http doesn't
	// expose APIs that low-level.) In net/http's ResponseWriter API, we do that
	// by writing to the headers map with a special prefix. This is purely an
	// implementation detail, so we should hide it and _not_ mutate the
	// user-visible headers.
	//
	// Note that this is _very_ finicky, and it's impossible to test with a
	// net/http client. Breaking this logic breaks Envoy's gRPC-Web translation.
	for key, values := range mergedTrailers {
		for _, value := range values {
			hs.writer.Header().Add(http.TrailerPrefix+key, value)
		}
	}
	return nil
}

func (hs *handlerSender) Spec() Specification {
	return hs.spec
}

func (hs *handlerSender) Header() http.Header {
	return hs.header
}

func (hs *handlerSender) Trailer() http.Header {
	return hs.trailer
}

func (hs *handlerSender) flush() {
	if f, ok := hs.writer.(http.Flusher); ok {
		f.Flush()
	}
}

type handlerReceiver struct {
	spec        Specification
	unmarshaler unmarshaler
	request     *http.Request
}

var _ Receiver = (*handlerReceiver)(nil)

func (hr *handlerReceiver) Receive(message any) error {
	if err := hr.unmarshaler.Unmarshal(message); err != nil {
		if errors.Is(err, errGotWebTrailers) {
			if hr.request.Trailer == nil {
				hr.request.Trailer = hr.unmarshaler.WebTrailer()
			} else {
				mergeHeaders(hr.request.Trailer, hr.unmarshaler.WebTrailer())
			}
		}
		return err // already coded
	}
	// don't return typed nils
	return nil
}

func (hr *handlerReceiver) Close() error {
	// We don't want to copy unread portions of the body to /dev/null here: if
	// the client hasn't closed the request body, we'll block until the server
	// timeout kicks in. This could happen because the client is malicious, but
	// a well-intentioned client may just not expect the server to be returning
	// an error for a streaming RPC. Better to accept that we can't always reuse
	// TCP connections.
	if err := hr.request.Body.Close(); err != nil {
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		return NewError(CodeUnknown, err)
	}
	return nil
}

func (hr *handlerReceiver) Spec() Specification {
	return hr.spec
}

func (hr *handlerReceiver) Header() http.Header {
	return hr.request.Header
}

func (hr *handlerReceiver) Trailer() http.Header {
	return hr.request.Trailer
}
