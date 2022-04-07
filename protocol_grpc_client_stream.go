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
	"strconv"
	"sync"
	"time"

	statusv1 "connectrpc.com/connect/internal/gen/go/connectext/grpc/status/v1"
)

// See duplexClientStream below: the send and receive sides of client streams
// are tightly interconnected, so it's simpler to implement the Sender
// interface as a facade over a full-duplex stream.
type clientSender struct {
	stream *duplexClientStream
}

var _ Sender = (*clientSender)(nil)

func (cs *clientSender) Send(m any) error      { return cs.stream.Send(m) }
func (cs *clientSender) Close(err error) error { return cs.stream.CloseSend(err) }
func (cs *clientSender) Spec() Specification   { return cs.stream.Spec() }
func (cs *clientSender) Header() http.Header   { return cs.stream.Header() }
func (cs *clientSender) Trailer() http.Header  { return cs.stream.Trailer() }

// See duplexClientStream below: the send and receive sides of client streams
// are tightly interconnected, so it's simpler to implement the Receiver
// interface as a facade over a full-duplex stream.
type clientReceiver struct {
	stream *duplexClientStream
}

var _ Receiver = (*clientReceiver)(nil)

func (cr *clientReceiver) Receive(m any) error  { return cr.stream.Receive(m) }
func (cr *clientReceiver) Close() error         { return cr.stream.CloseReceive() }
func (cr *clientReceiver) Spec() Specification  { return cr.stream.Spec() }
func (cr *clientReceiver) Header() http.Header  { return cr.stream.ResponseHeader() }
func (cr *clientReceiver) Trailer() http.Header { return cr.stream.ResponseTrailer() }

// duplexClientStream is a bidirectional exchange of Protobuf messages between
// the client and server. The request body is the stream from client to server,
// and the response body is the reverse.
//
// The way we do this with net/http is very different from the typical HTTP/1.1
// request/response code. Since this is the most complex code in connect, it has
// many more comments than usual.
type duplexClientStream struct {
	ctx        context.Context
	httpClient HTTPClient
	url        string
	spec       Specification
	codec      Codec
	protobuf   Codec // for errors

	// send: guarded by prepareOnce because we can't initialize this state until
	// the first call to Send.
	prepareOnce sync.Once
	writer      *io.PipeWriter
	marshaler   marshaler
	header      http.Header
	trailer     http.Header

	// receive goroutine
	web              bool
	reader           *io.PipeReader
	response         *http.Response
	responseHeader   http.Header
	responseTrailer  http.Header
	responseReady    chan struct{}
	unmarshaler      unmarshaler
	compressionPools readOnlyCompressionPools

	errMu       sync.Mutex
	requestErr  error
	responseErr error
}

func (cs *duplexClientStream) Spec() Specification {
	return cs.spec
}

func (cs *duplexClientStream) Header() http.Header {
	return cs.header
}

func (cs *duplexClientStream) Trailer() http.Header {
	return cs.trailer
}

func (cs *duplexClientStream) Send(message any) error {
	cs.prepareRequests()
	// Before we receive the message, check if the context has been canceled.
	if err := cs.ctx.Err(); err != nil {
		cs.setResponseError(err)
		return err
	}
	// Calling Marshal writes data to the send stream. It's safe to do this while
	// makeRequest is running, because we're writing to our side of the pipe
	// (which is safe to do while net/http reads from the other side).
	if err := cs.marshaler.Marshal(message); err != nil {
		if !errors.Is(err, io.ErrClosedPipe) {
			return err
		}
		// In all other cases, we return a io.EOF, similar to gRPC and the user
		// will get the state of the stream through Receive.
		return NewError(CodeUnknown, io.EOF)
	}
	return nil
}

func (cs *duplexClientStream) CloseSend(_ error) error {
	// Even if Send was never called, we need to make an HTTP request. This ensures
	// that we've sent any headers to the server and that we got an HTTP response body
	// from the server for Receive to unmarshal from if called.
	cs.prepareRequests()
	// The user calls CloseSend to indicate that they're done sending data. All
	// we do here is write to the pipe and close it, so it's safe to do this
	// while makeRequest is running. (This method takes an error to accommodate
	// server-side streams. Clients can't send an error when they stop sending
	// data, so we just ignore it.)
	//
	// Because connect also supports some RPC types over HTTP/1.1, we need to be
	// careful how we expose this method to users. HTTP/1.1 doesn't support
	// bidirectional streaming - the send stream (aka request body) must be
	// closed before we start waiting on the response or we'll just block
	// forever. To make sure users don't have to worry about this, the generated
	// code for unary, client streaming, and server streaming RPCs must call
	// CloseSend automatically rather than requiring the user to do it.
	if cs.web {
		if err := cs.marshaler.MarshalWebTrailers(cs.trailer); err != nil {
			if !errors.Is(err, io.ErrClosedPipe) {
				_ = cs.writer.Close()
				return err
			}
		}
	}
	if err := cs.writer.Close(); err != nil {
		if connectErr, ok := asError(err); ok {
			return connectErr
		}
		return NewError(CodeUnknown, err)
	}
	return nil
}

func (cs *duplexClientStream) Receive(message any) error {
	// First, we wait until we've gotten the response headers and established the
	// server-to-client side of the stream.
	<-cs.responseReady
	if err := cs.getRequestOrResponseError(); err != nil {
		// The stream is already closed or corrupted.
		return err
	}
	// Before we receive the message, check if the context has been canceled.
	if err := cs.ctx.Err(); err != nil {
		cs.setResponseError(err)
		return err
	}
	// Consume one message from the response stream.
	err := cs.unmarshaler.Unmarshal(message)
	if err != nil {
		// If we can't read this LPM, see if the server sent an explicit error in
		// trailers. First, we need to read the body to EOF.
		_ = discard(cs.response.Body)
		mergeHeaders(cs.responseTrailer, cs.response.Trailer)
		if errors.Is(err, errGotWebTrailers) {
			mergeHeaders(cs.responseTrailer, cs.unmarshaler.WebTrailer())
		}
		if serverErr := extractError(cs.protobuf, cs.responseTrailer); serverErr != nil {
			// This is expected from a protocol perspective, but receiving trailers
			// means that we're _not_ getting a message. For users to realize that
			// the stream has ended, Receive must return an error.
			serverErr.meta = cs.responseHeader.Clone()
			mergeHeaders(serverErr.meta, cs.responseTrailer)
			cs.setResponseError(serverErr)
			return serverErr
		}
		// There's no error in the trailers, so this was probably an error
		// converting the bytes to a message, an error reading from the network, or
		// just an EOF. We're going to return it to the user, but we also want to
		// setResponseError so Send errors out.
		cs.setResponseError(err)
		return err
	}
	return nil
}

func (cs *duplexClientStream) CloseReceive() error {
	<-cs.responseReady
	if cs.response == nil {
		return nil
	}
	if err := discard(cs.response.Body); err != nil {
		return NewError(CodeUnknown, err)
	}
	if err := cs.response.Body.Close(); err != nil {
		return NewError(CodeUnknown, err)
	}
	return nil
}

func (cs *duplexClientStream) ResponseHeader() http.Header {
	<-cs.responseReady
	return cs.responseHeader
}

func (cs *duplexClientStream) ResponseTrailer() http.Header {
	<-cs.responseReady
	return cs.responseTrailer
}

// stream.makeRequest hands the read side of the pipe off to net/http and
// waits to establish the response stream. There's a small class of errors we
// can catch before writing to the request body, so we don't want to start
// writing to the stream until we're sure that we're actually waiting on the
// server. This makes user-visible behavior more predictable: for example, if
// they've configured the server's base URL as "hwws://acme.com", they'll
// always get an invalid URL error on their first attempt to send.
func (cs *duplexClientStream) prepareRequests() {
	cs.prepareOnce.Do(func() {
		requestPrepared := make(chan struct{})
		go cs.makeRequest(requestPrepared)
		<-requestPrepared
	})
}

func (cs *duplexClientStream) makeRequest(prepared chan struct{}) {
	// This runs concurrently with Send and CloseSend. Receive and CloseReceive
	// wait on cs.responseReady, so we can't race with them.
	defer close(cs.responseReady)

	if deadline, ok := cs.ctx.Deadline(); ok {
		if encodedDeadline, err := encodeTimeout(time.Until(deadline)); err == nil {
			// Tests verify that the error in encodeTimeout is unreachable, so we
			// should be safe without observability for the error case.
			cs.header["Grpc-Timeout"] = []string{encodedDeadline}
		}
	}

	req, err := http.NewRequestWithContext(cs.ctx, http.MethodPost, cs.url, cs.reader)
	if err != nil {
		cs.setRequestError(errorf(CodeUnknown, "construct *http.Request: %w", err))
		close(prepared)
		return
	}
	req.Header = cs.header
	if cs.trailer != nil && !cs.web {
		req.Trailer = cs.trailer
	}

	// At this point, we've caught all the errors we can - it's time to send data
	// to the server. Unblock Send.
	close(prepared)

	// Once we send a message to the server, they send a message back and
	// establish the receive side of the stream.
	res, err := cs.httpClient.Do(req)
	if err != nil {
		cs.setResponseError(err)
		return
	}

	if res.StatusCode != http.StatusOK {
		cs.setResponseError(errorf(httpToCode(res.StatusCode), "HTTP status %v", res.Status))
		return
	}
	compression := res.Header.Get("Grpc-Encoding")
	if compression == "" || compression == compressionIdentity {
		compression = compressionIdentity
	} else if !cs.compressionPools.Contains(compression) {
		// Per https://github.com/grpc/grpc/blob/master/doc/compression.md, we
		// should return CodeInternal and specify acceptable compression(s) (in
		// addition to setting the Grpc-Accept-Encoding header).
		cs.setResponseError(errorf(
			CodeInternal,
			"unknown compression %q: accepted grpc-encoding values are %v",
			compression,
			cs.compressionPools.CommaSeparatedNames(),
		))
		return
	}
	// When there's no body, gRPC-Web servers _may_ send error information in the
	// HTTP headers.
	//
	// From our perspective, standard gRPC servers also send error information in
	// the HTTP headers when they send "trailers-only" responses (which they do
	// when unary RPCs return errors, or when streaming RPCs return errors
	// immediately). The gRPC HTTP/2 specification says that "...Trailers-Only
	// are...delivered in a single HTTP2 HEADERS frame block. For responses
	// end-of-stream is indicated by the presence of the END_STREAM flag on the
	// last received HEADERS frame that carries Trailers." The final bit - that
	// HEADERS frames with EOS set should be treated as gRPC trailers, even if no
	// DATA frames have been sent on the stream - isn't standard HTTP/2
	// semantics, so net/http doesn't know anything about it. To us, then, these
	// trailers-only responses actually appear as headers-only responses.
	if err := extractError(cs.protobuf, res.Header); err != nil {
		// Per the specification, only the HTTP status code and Content-Type should
		// be treated as headers. The rest should be treated as trailing metadata.
		if contentType := res.Header.Get("Content-Type"); contentType != "" {
			cs.responseHeader.Set("Content-Type", contentType)
		}
		mergeHeaders(cs.responseTrailer, res.Header)
		cs.responseTrailer.Del("Content-Type")
		// If we get some actual HTTP trailers, treat those as trailing metadata too.
		_ = discard(res.Body)
		mergeHeaders(cs.responseTrailer, res.Trailer)
		// Merge HTTP headers and trailers into the error metadata.
		err.meta = res.Header.Clone()
		mergeHeaders(err.meta, res.Trailer)
		cs.setResponseError(err)
		return
	}
	// Success! We got a response with valid headers and no error, so there's
	// probably a message waiting in the stream.
	cs.response = res
	mergeHeaders(cs.responseHeader, res.Header)
	cs.unmarshaler = unmarshaler{
		reader:          res.Body,
		codec:           cs.codec,
		compressionPool: cs.compressionPools.Get(compression),
		web:             cs.web,
	}
}

func (cs *duplexClientStream) setRequestError(err error) {
	cs.setError(err, true /* isRequest */)
}

func (cs *duplexClientStream) setResponseError(err error) {
	cs.setError(err, false /* isRequest */)
}

func (cs *duplexClientStream) setError(err error, isRequest bool) {
	// Normally, we can rely on our built-in middleware to add codes to
	// context-related errors. However, errors set here are exposed on the
	// io.Writer end of the pipe, where they'll likely be miscoded if they're not
	// already wrapped.
	err = wrapIfContextError(err)

	cs.errMu.Lock()
	if isRequest {
		cs.requestErr = err
	} else {
		cs.responseErr = err
	}
	// The next call (cs.reader.Close) also needs to acquire an internal
	// lock, so we want to scope errMu's usage narrowly and avoid defer.
	cs.errMu.Unlock()

	// We've already hit an error, so we should stop writing to the request body.
	// It's safe to call Close more than once and/or concurrently (calls after
	// the first are no-ops), so it's okay for us to call this even though
	// net/http sometimes closes the reader too.
	//
	// It's safe to ignore the returned error here. Under the hood, Close calls
	// CloseWithError, which is documented to always return nil.
	_ = cs.reader.Close()
}

func (cs *duplexClientStream) getRequestError() error {
	cs.errMu.Lock()
	defer cs.errMu.Unlock()
	return cs.requestErr
}

func (cs *duplexClientStream) getRequestOrResponseError() error {
	cs.errMu.Lock()
	defer cs.errMu.Unlock()
	if cs.requestErr != nil {
		return cs.requestErr
	}
	return cs.responseErr
}

// The gRPC wire protocol specifies that errors should be serialized using the
// binary Protobuf format, even if the messages in the request/response stream
// use a different codec. Consequently, this function needs a Protobuf codec to
// unmarshal error information in the headers.
func extractError(protobuf Codec, trailer http.Header) *Error {
	codeHeader := trailer.Get("Grpc-Status")
	if codeHeader == "" || codeHeader == "0" {
		return nil
	}

	code, err := strconv.ParseUint(codeHeader, 10 /* base */, 32 /* bitsize */)
	if err != nil {
		return errorf(CodeUnknown, "gRPC protocol error: got invalid error code %q", codeHeader)
	}
	message := percentDecode(trailer.Get("Grpc-Message"))
	retErr := NewError(Code(code), errors.New(message))

	detailsBinaryEncoded := trailer.Get("Grpc-Status-Details-Bin")
	if len(detailsBinaryEncoded) > 0 {
		detailsBinary, err := DecodeBinaryHeader(detailsBinaryEncoded)
		if err != nil {
			return errorf(CodeUnknown, "server returned invalid grpc-status-details-bin trailer: %w", err)
		}
		var status statusv1.Status
		if err := protobuf.Unmarshal(detailsBinary, &status); err != nil {
			return errorf(CodeUnknown, "server returned invalid protobuf for error details: %w", err)
		}
		for _, d := range status.Details {
			retErr.details = append(retErr.details, d)
		}
		// Prefer the Protobuf-encoded data to the headers (grpc-go does this too).
		retErr.code = Code(status.Code)
		retErr.err = errors.New(status.Message)
	}

	return retErr
}
