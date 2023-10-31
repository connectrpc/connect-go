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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
)

// httpCall builds a HTTP request and sends it to a servcer. Unary calls block
// until the response is received. Streaming calls return immediately.
type httpCall struct {
	ctx              context.Context
	client           HTTPClient
	streamType       StreamType
	onRequestSend    func(*http.Request)
	validateResponse func(*http.Response) *Error

	// io.Pipe is used to implement the request body for client streaming calls.
	// If the request is unary, this is nil.
	requestBodyWriter *io.PipeWriter

	requestSent atomic.Bool
	request     *http.Request

	responseReady chan struct{}
	response      *http.Response
	responseErr   error
}

func newHTTPCall(
	ctx context.Context,
	client HTTPClient,
	url *url.URL,
	spec Spec,
	header http.Header,
) *httpCall {
	// ensure we make a copy of the url before we pass along to the
	// Request. This ensures if a transport out of our control wants
	// to mutate the req.URL, we don't feel the effects of it.
	url = cloneURL(url)
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
		Body:       http.NoBody,
		Host:       url.Host,
	}).WithContext(ctx)
	return &httpCall{
		ctx:           ctx,
		client:        client,
		streamType:    spec.StreamType,
		request:       request,
		responseReady: make(chan struct{}),
	}
}

// Send the request headers and body. If the streamType is not client streaming,
// this method blocks until the response headers are received.
func (c *httpCall) Send(buffer *bytes.Buffer) error {
	// Before we send any data, check if the context has been canceled.
	if err := c.ctx.Err(); err != nil {
		return wrapIfContextError(err)
	}
	if c.isClientStream() {
		return c.sendStream(buffer)
	}
	return c.sendUnary(buffer)
}

// Close the request body. Callers *must* call CloseWrite before Read when
// using HTTP/1.x.
func (c *httpCall) CloseWrite() error {
	// Even if Send was never called, we need to make an HTTP request. This
	// ensures that we've sent any headers to the server and that we have an HTTP
	// response to read from.
	if c.requestSent.CompareAndSwap(false, true) {
		go c.makeRequest()
	}
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
	if c.requestBodyWriter != nil {
		return c.requestBodyWriter.Close()
	}
	return c.request.Body.Close()
}

// Header returns the HTTP request headers.
func (c *httpCall) Header() http.Header {
	return c.request.Header
}

// URL returns the URL for the request.
func (c *httpCall) URL() *url.URL {
	return c.request.URL
}

// SetMethod changes the method of the request before it is sent.
func (c *httpCall) SetMethod(method string) {
	c.request.Method = method
}

// Read from the response body.
func (c *httpCall) Read(data []byte) (int, error) {
	// First, we wait until we've gotten the response headers and established the
	// server-to-client side of the stream.
	if err := c.BlockUntilResponseReady(); err != nil {
		return 0, err
	}
	// Before we read, check if the context has been canceled.
	if err := c.ctx.Err(); err != nil {
		return 0, wrapIfContextError(err)
	}
	n, err := c.response.Body.Read(data)
	return n, wrapIfRSTError(err)
}

func (c *httpCall) CloseRead() error {
	_ = c.BlockUntilResponseReady()
	if c.response == nil {
		return nil
	}
	if _, err := discard(c.response.Body); err != nil {
		_ = c.response.Body.Close()
		return wrapIfRSTError(err)
	}
	return wrapIfRSTError(c.response.Body.Close())
}

// ResponseStatusCode is the response's HTTP status code.
func (c *httpCall) ResponseStatusCode() (int, error) {
	if err := c.BlockUntilResponseReady(); err != nil {
		return 0, err
	}
	return c.response.StatusCode, nil
}

// ResponseHeader returns the response HTTP headers.
func (c *httpCall) ResponseHeader() http.Header {
	_ = c.BlockUntilResponseReady()
	if c.response != nil {
		return c.response.Header
	}
	return make(http.Header)
}

// ResponseTrailer returns the response HTTP trailers.
func (c *httpCall) ResponseTrailer() http.Header {
	_ = c.BlockUntilResponseReady()
	if c.response != nil {
		return c.response.Trailer
	}
	return make(http.Header)
}

// SetValidateResponse sets the response validation function. The function runs
// in a background goroutine.
func (c *httpCall) SetValidateResponse(validate func(*http.Response) *Error) {
	c.validateResponse = validate
}

func (c *httpCall) BlockUntilResponseReady() error {
	<-c.responseReady
	return c.responseErr
}

func (c *httpCall) isClientStream() bool {
	return c.streamType&StreamTypeClient != 0
}

func (c *httpCall) sendUnary(buffer *bytes.Buffer) error {
	if !c.requestSent.CompareAndSwap(false, true) {
		return fmt.Errorf("unary call already sent")
	}
	// Client request is unary, build the full request body and block on
	// sending the request.
	if c.request.Method != http.MethodGet && buffer != nil {
		payload := newPayloadCloser(buffer)
		c.request.Body = payload
		c.request.ContentLength = int64(buffer.Len())
		c.request.GetBody = func() (io.ReadCloser, error) {
			if payload.Rewind() {
				return payload, nil
			}
			return nil, errors.New("payload cannot be rewound")
		}
		// Wait for the payload to be fully drained before we return
		// from Send to ensure the buffer can safely be reused.
		defer payload.Wait()
	}
	c.makeRequest() // Blocks until the response is ready
	return nil      // Only report response errors on Read
}

func (c *httpCall) sendStream(buffer *bytes.Buffer) error {
	if c.requestSent.CompareAndSwap(false, true) {
		// Client request is streaming, so we need to start sending the request
		// before we start writing to the request body. This ensures that we've
		// sent any headers to the server.
		pipeReader, pipeWriter := io.Pipe()
		c.requestBodyWriter = pipeWriter
		c.request.Body = pipeReader
		c.request.ContentLength = -1
		go c.makeRequest() // concurrent request
	}
	// It's safe to write to this side of the pipe while net/http concurrently
	// reads from the other side.
	_, err := c.requestBodyWriter.Write(buffer.Bytes())
	if err != nil && errors.Is(err, io.ErrClosedPipe) {
		// Signal that the stream is closed with the more-typical io.EOF instead of
		// io.ErrClosedPipe. This makes it easier for protocol-specific wrappers to
		// match grpc-go's behavior.
		return io.EOF
	}
	return err
}

func (c *httpCall) makeRequest() {
	// This runs concurrently with Write and CloseWrite. Read and CloseRead wait
	// on d.responseReady, so we can't race with them.
	defer close(c.responseReady)

	// Promote the header Host to the request object.
	if host := c.request.Header.Get(headerHost); len(host) > 0 {
		c.request.Host = host
	}

	if c.onRequestSend != nil {
		c.onRequestSend(c.request)
	}
	// Once we send a message to the server, they send a message back and
	// establish the receive side of the stream.
	response, err := c.client.Do(c.request)
	if err != nil {
		err = wrapIfContextError(err)
		err = wrapIfLikelyH2CNotConfiguredError(c.request, err)
		err = wrapIfLikelyWithGRPCNotUsedError(err)
		err = wrapIfRSTError(err)
		if _, ok := asError(err); !ok {
			err = NewError(CodeUnavailable, err)
		}
		c.responseErr = err
		if c.requestBodyWriter != nil {
			c.requestBodyWriter.Close()
		}
		return
	}
	c.response = response
	if err := c.validateResponse(response); err != nil {
		c.responseErr = err
		if c.requestBodyWriter != nil {
			c.requestBodyWriter.Close()
		}
		return
	}
	if (c.streamType&StreamTypeBidi) == StreamTypeBidi && response.ProtoMajor < 2 {
		// If we somehow dialed an HTTP/1.x server, fail with an explicit message
		// rather than returning a more cryptic error later on.
		c.responseErr = errorf(
			CodeUnimplemented,
			"response from %v is HTTP/%d.%d: bidi streams require at least HTTP/2",
			c.request.URL,
			response.ProtoMajor,
			response.ProtoMinor,
		)
		if c.requestBodyWriter != nil {
			c.requestBodyWriter.Close()
		}
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

// payloadCloser is a ReadCloser that wraps a bytes.Buffer. It's used to
// implement the request body for unary calls. To safely reuse the buffer
// call Wait after the response is received to ensure the payload has been
// fully read.
type payloadCloser struct {
	wait sync.WaitGroup

	mu     sync.Mutex
	buf    *bytes.Buffer
	readN  int
	isDone bool // true if the payload has been fully read
}

func newPayloadCloser(buf *bytes.Buffer) *payloadCloser {
	payload := &payloadCloser{
		buf: buf,
	}
	payload.wait.Add(1) // Wait until complete is called
	return payload
}

// Read implements io.Reader.
func (p *payloadCloser) Read(dst []byte) (readN int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buf == nil {
		return 0, io.EOF
	}
	src := p.buf.Bytes()
	readN = copy(dst, src[p.readN:])
	p.readN += readN
	if readN == 0 && p.readN == len(src) {
		p.completeWithLock()
		err = io.EOF
	}
	return readN, err
}

// Close implements io.Closer. It signals that the payload has been fully read.
func (p *payloadCloser) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completeWithLock()
	return nil
}

// Rewind resets the payload to the beginning. It returns false if the buffer
// has been discarded from a previous call to Wait.
func (p *payloadCloser) Rewind() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buf == nil {
		return false
	}
	p.readN = 0
	if p.isDone {
		p.isDone = false
		p.wait.Add(1)
	}
	return true
}

// Wait blocks until the payload has been fully read.
func (p *payloadCloser) Wait() {
	p.wait.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = nil // Discard the buffer
}

// complete signals that the payload has been fully read.
func (p *payloadCloser) completeWithLock() {
	if !p.isDone {
		p.isDone = true
		p.wait.Done()
	}
}
