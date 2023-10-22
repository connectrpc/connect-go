// Copyright 2021-2023 The Connect Authors
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
	"sync"
)

// duplexHTTPCall is a full-duplex stream between the client and server. The
// request body is the stream from client to server, and the response body is
// the reverse.
//
// Be warned: we need to use some lesser-known APIs to do this with net/http.
type duplexHTTPCall struct {
	ctx              context.Context
	httpClient       HTTPClient
	streamType       StreamType
	onRequestSend    func(*http.Request)
	validateResponse func(*http.Response) *Error

	// We'll use a pipe as the request body. We hand the read side of the pipe to
	// net/http, and we write to the write side (naturally). The two ends are
	// safe to use concurrently.
	requestBodyReader *io.PipeReader
	requestBodyWriter *io.PipeWriter

	requestSent   bool
	responseReady sync.WaitGroup
	request       *http.Request
	response      *http.Response
	responseErr   error
}

func newDuplexHTTPCall(
	ctx context.Context,
	httpClient HTTPClient,
	url *url.URL,
	spec Spec,
	header http.Header,
) *duplexHTTPCall {
	// ensure we make a copy of the url before we pass along to the
	// Request. This ensures if a transport out of our control wants
	// to mutate the req.URL, we don't feel the effects of it.
	url = cloneURL(url)
	pipeReader, pipeWriter := io.Pipe()

	// This is mirroring what http.NewRequestContext did, but
	// using an already parsed url.URL object, rather than a string
	// and parsing it again. This is a bit funny with HTTP/1.1
	// explicitly, but this is logic copied over from
	// NewRequestContext and doesn't effect the actual version
	// being transmitted.
	request := (&http.Request{
		Method:     http.MethodPost,
		URL:        url,
		Header:     header,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Body:       pipeReader,
		Host:       url.Host,
	}).WithContext(ctx)
	call := &duplexHTTPCall{
		ctx:               ctx,
		httpClient:        httpClient,
		streamType:        spec.StreamType,
		requestBodyReader: pipeReader,
		requestBodyWriter: pipeWriter,
		request:           request,
	}
	call.responseReady.Add(1)
	return call
}

// Write to the request body.
func (d *duplexHTTPCall) Write(data []byte) (int, error) {
	d.ensureRequestMade()
	// Before we send any data, check if the context has been canceled.
	if err := d.ctx.Err(); err != nil {
		return 0, wrapIfContextError(err)
	}
	// It's safe to write to this side of the pipe while net/http concurrently
	// reads from the other side.
	bytesWritten, err := d.requestBodyWriter.Write(data)
	if err != nil && errors.Is(err, io.ErrClosedPipe) {
		// Signal that the stream is closed with the more-typical io.EOF instead of
		// io.ErrClosedPipe. This makes it easier for protocol-specific wrappers to
		// match grpc-go's behavior.
		return bytesWritten, io.EOF
	}
	return bytesWritten, err
}

// Close the request body. Callers *must* call CloseWrite before Read when
// using HTTP/1.x.
func (d *duplexHTTPCall) CloseWrite() error {
	// Even if Write was never called, we need to make an HTTP request. This
	// ensures that we've sent any headers to the server and that we have an HTTP
	// response to read from.
	d.ensureRequestMade()
	// The user calls CloseWrite to indicate that they're done sending data. It's
	// safe to close the write side of the pipe while net/http is reading from
	// it.
	//
	// Because connect also supports some RPC types over HTTP/1.1, we need to be
	// careful how we expose this method to users. HTTP/1.1 doesn't support
	// bidirectional streaming - the write side of the stream (aka request body)
	// must be closed before we start reading the response or we'll just block
	// forever. To make sure users don't have to worry about this, the generated
	// code for unary, client streaming, and server streaming RPCs must call
	// CloseWrite automatically rather than requiring the user to do it.
	return d.requestBodyWriter.Close()
}

// Header returns the HTTP request headers.
func (d *duplexHTTPCall) Header() http.Header {
	return d.request.Header
}

// Trailer returns the HTTP request trailers.
func (d *duplexHTTPCall) Trailer() http.Header {
	return d.request.Trailer
}

// URL returns the URL for the request.
func (d *duplexHTTPCall) URL() *url.URL {
	return d.request.URL
}

// SetMethod changes the method of the request before it is sent.
func (d *duplexHTTPCall) SetMethod(method string) {
	d.request.Method = method
}

// Read from the response body. Returns the first error passed to SetError.
func (d *duplexHTTPCall) Read(data []byte) (int, error) {
	// First, we wait until we've gotten the response headers and established the
	// server-to-client side of the stream.
	if err := d.BlockUntilResponseReady(); err != nil {
		// The stream is already closed or corrupted.
		return 0, err
	}
	// Before we read, check if the context has been canceled.
	if err := d.ctx.Err(); err != nil {
		return 0, wrapIfContextError(err)
	}
	if d.response == nil {
		return 0, fmt.Errorf("nil response from %v", d.request.URL)
	}
	n, err := d.response.Body.Read(data)
	return n, wrapIfRSTError(err)
}

func (d *duplexHTTPCall) CloseRead() error {
	d.responseReady.Wait()
	if d.response == nil {
		return nil
	}
	if _, err := discard(d.response.Body); err != nil {
		_ = d.response.Body.Close()
		return wrapIfRSTError(err)
	}
	return wrapIfRSTError(d.response.Body.Close())
}

// ResponseStatusCode is the response's HTTP status code.
func (d *duplexHTTPCall) ResponseStatusCode() (int, error) {
	if err := d.BlockUntilResponseReady(); err != nil {
		return 0, err
	}
	if d.response == nil {
		return 0, fmt.Errorf("nil response from %v", d.request.URL)
	}
	return d.response.StatusCode, nil
}

// ResponseHeader returns the response HTTP headers.
func (d *duplexHTTPCall) ResponseHeader() http.Header {
	d.responseReady.Wait()
	if d.response != nil {
		return d.response.Header
	}
	return make(http.Header)
}

// ResponseTrailer returns the response HTTP trailers.
func (d *duplexHTTPCall) ResponseTrailer() http.Header {
	d.responseReady.Wait()
	if d.response != nil {
		return d.response.Trailer
	}
	return make(http.Header)
}

// SetValidateResponse sets the response validation function. The function runs
// in a background goroutine.
func (d *duplexHTTPCall) SetValidateResponse(validate func(*http.Response) *Error) {
	d.validateResponse = validate
}

// BlockUntilResponseReady returns when the response is ready or reports an
// error from initializing the request.
func (d *duplexHTTPCall) BlockUntilResponseReady() error {
	d.responseReady.Wait()
	return d.responseErr
}

// ensureRequestMade sends the request headers and starts the response stream.
// It is not safe to call this concurrently. Write and CloseWrite call this but
// ensure that they're not called concurrently.
func (d *duplexHTTPCall) ensureRequestMade() {
	if d.requestSent {
		return // already sent
	}
	d.requestSent = true
	go d.makeRequest()
}

func (d *duplexHTTPCall) makeRequest() {
	// This runs concurrently with Write and CloseWrite. Read and CloseRead wait
	// on d.responseReady, so we can't race with them.
	defer d.responseReady.Done()

	// Promote the header Host to the request object.
	if host := d.request.Header.Get(headerHost); len(host) > 0 {
		d.request.Host = host
	}

	if d.onRequestSend != nil {
		d.onRequestSend(d.request)
	}
	// Once we send a message to the server, they send a message back and
	// establish the receive side of the stream.
	response, err := d.httpClient.Do(d.request) //nolint:bodyclose
	if err != nil {
		err = wrapIfContextError(err)
		err = wrapIfLikelyH2CNotConfiguredError(d.request, err)
		err = wrapIfLikelyWithGRPCNotUsedError(err)
		err = wrapIfRSTError(err)
		if _, ok := asError(err); !ok {
			err = NewError(CodeUnavailable, err)
		}
		d.responseErr = err
		d.requestBodyReader.CloseWithError(io.EOF)
		return
	}
	d.response = response
	if err := d.validateResponse(response); err != nil {
		d.responseErr = err
		d.response.Body.Close()
		d.requestBodyReader.CloseWithError(io.EOF)
		return
	}
	if (d.streamType&StreamTypeBidi) == StreamTypeBidi && response.ProtoMajor < 2 {
		// If we somehow dialed an HTTP/1.x server, fail with an explicit message
		// rather than returning a more cryptic error later on.
		d.responseErr = errorf(
			CodeUnimplemented,
			"response from %v is HTTP/%d.%d: bidi streams require at least HTTP/2",
			d.request.URL,
			response.ProtoMajor,
			response.ProtoMinor,
		)
		d.response.Body.Close()
		d.requestBodyReader.CloseWithError(io.EOF)
	}
}

// See: https://cs.opensource.google/go/go/+/refs/tags/go1.20.1:src/net/http/clone.go;l=22-33
func cloneURL(oldURL *url.URL) *url.URL {
	if oldURL == nil {
		return nil
	}
	newURL := new(url.URL)
	*newURL = *oldURL
	if oldURL.User != nil {
		newURL.User = new(url.Userinfo)
		*newURL.User = *oldURL.User
	}
	return newURL
}
