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

package connect

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	connectv2 "connectrpc.com/connect/v2"
)

// clientStreamConn projects a v2 [connectv2.ClientStream] onto the v1
// [StreamingClientConn] interface. It owns the request-header http.Header so
// v1 callers can mutate it before the first Send; the wrapper copies the
// header into the underlying v2 CallInfo on first dispatch.
type clientStreamConn struct {
	// ctx is the stream's context. v1's conn methods take no context, so it's
	// held to report cancellation before each Send and Receive.
	ctx      context.Context //nolint:containedctx
	protocol string
	spec     Spec
	peer     Peer
	stream   connectv2.ClientStream
	info     *clientCallInfo

	mu             sync.Mutex
	startedSending bool
	closedSend     bool
	closedRecv     bool
	// responseHeader and responseTrailer are stable maps returned across
	// calls so v1 interceptors that climb the response onion can decorate
	// them in place and have the next interceptor up observe the addition.
	// Each access re-syncs v2's current metadata into the map (see
	// refreshHeaderFromMetadata), so a read before the transport has
	// populated the metadata sees the headers later instead of freezing the
	// empty map it observed first.
	responseHeader  http.Header
	responseTrailer http.Header
}

func (c *clientStreamConn) Spec() Spec { return c.spec }
func (c *clientStreamConn) Peer() Peer { return c.peer }

func (c *clientStreamConn) RequestHeader() http.Header {
	if c.info != nil {
		return c.info.RequestHeader()
	}
	return http.Header{}
}

func (c *clientStreamConn) Send(msg any) error {
	if err := c.ctx.Err(); err != nil {
		return wrapIfContextError(err)
	}
	c.flushRequestHeader()
	// v1 callers invoke Send(nil) to flush request headers without yet
	// having a message ready. v2 expects a non-nil message in Send, so
	// translate the nil call to a Start.
	if msg == nil {
		return v1ErrorOnly(c.stream.SendHeaders())
	}
	if err := c.stream.Send(msg); err != nil {
		return v1ErrorOnly(err)
	}
	return nil
}

func (c *clientStreamConn) CloseRequest() error {
	c.flushRequestHeader()
	c.mu.Lock()
	if c.closedSend {
		c.mu.Unlock()
		return nil
	}
	c.closedSend = true
	c.mu.Unlock()
	return v1ErrorOnly(c.stream.CloseSend())
}

func (c *clientStreamConn) Receive(msg any) error {
	if err := c.ctx.Err(); err != nil {
		return wrapIfContextError(err)
	}
	c.flushRequestHeader()
	err := v1ErrorOnly(c.stream.Receive(msg))
	// Bidi streams require HTTP/2. v2's transport allows bidi over HTTP/1
	// (the conformance suite exercises half-duplex bidi there), but v1's
	// contract is to reject it, so re-apply the check here once the response
	// version is known rather than surfacing a confusing decode error.
	if bidiErr := c.bidiHTTPVersionError(); bidiErr != nil {
		return bidiErr
	}
	if err == nil || err == io.EOF { //nolint:errorlint // exact io.EOF identity
		return err
	}
	c.projectTrailersOntoError(err)
	return err
}

// bidiHTTPVersionError reports a v1-style error when a bidirectional stream
// lands on an HTTP/1.x response. A 200 means the peer accepted the stream it
// cannot fulfil over HTTP/1.x, so callers see CodeUnimplemented; any other
// status is surfaced as the HTTP status (e.g. a 505 from a v1 handler that
// refused the request). Returns nil for non-bidi RPCs, HTTP/2 responses, or
// before the response has arrived.
func (c *clientStreamConn) bidiHTTPVersionError() error {
	if c.spec.StreamType != StreamTypeBidi || c.info == nil || c.info.httpInfo == nil {
		return nil
	}
	proto := c.info.httpInfo.ResponseProto()
	if !strings.HasPrefix(proto, "HTTP/1") {
		return nil
	}
	if status := c.info.httpInfo.ResponseStatus(); status != http.StatusOK {
		return NewError(httpStatusToConnectCode(status), fmt.Errorf("HTTP status %d %s", status, http.StatusText(status)))
	}
	requestURL := c.info.httpInfo.RequestURL()
	return NewError(CodeUnimplemented, fmt.Errorf("response from %v is %s: bidi streams require at least HTTP/2", &requestURL, proto))
}

// httpStatusToConnectCode mirrors the HTTP-status-to-Code mapping the v2
// transport uses, for the v1 bidi-over-HTTP/1 rejection path.
func httpStatusToConnectCode(status int) Code {
	switch status {
	case http.StatusBadRequest:
		return CodeInternal
	case http.StatusUnauthorized:
		return CodeUnauthenticated
	case http.StatusForbidden:
		return CodePermissionDenied
	case http.StatusNotFound:
		return CodeUnimplemented
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return CodeUnavailable
	}
	return CodeUnknown
}

// projectTrailersOntoError copies the response trailers/headers onto a
// *Error.Meta() so streaming callers see metadata the server attached
// alongside its verdict. v1's unary client does the same in
// finishClientErr; streaming Receives need the explicit mirror because
// the v2 stream surfaces only the *Error, not the call info.
func (c *clientStreamConn) projectTrailersOntoError(err error) {
	verr, ok := asError(err)
	if !ok || c.info == nil {
		return
	}
	mergeCallInfoMetaIntoError(c.info.v2Info, verr)
}

func (c *clientStreamConn) ResponseHeader() http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.responseHeader == nil {
		c.responseHeader = http.Header{}
	}
	if c.info != nil && c.info.v2Info != nil {
		refreshHeaderFromMetadata(c.responseHeader, c.info.v2Info.ResponseHeader())
	}
	return c.responseHeader
}

func (c *clientStreamConn) ResponseTrailer() http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.responseTrailer == nil {
		c.responseTrailer = http.Header{}
	}
	if c.info != nil && c.info.v2Info != nil {
		refreshHeaderFromMetadata(c.responseTrailer, c.info.v2Info.ResponseTrailer())
	}
	return c.responseTrailer
}

// refreshHeaderFromMetadata overwrites dst's entry for every key present in
// src with src's current values, leaving keys absent from src (e.g. a
// decoration a v1 interceptor added in place) untouched. It is idempotent, so
// calling it on each access keeps the stable cached header in sync with v2's
// metadata as the transport populates it without dropping interceptor
// additions or duplicating server values.
func refreshHeaderFromMetadata(dst http.Header, src *connectv2.Header) {
	if src == nil {
		return
	}
	for key, vals := range src.All() {
		dst[key] = append([]string(nil), vals...)
	}
}

func (c *clientStreamConn) CloseResponse() error {
	c.mu.Lock()
	if c.closedRecv {
		c.mu.Unlock()
		return nil
	}
	c.closedRecv = true
	c.mu.Unlock()
	err := c.stream.Close()
	// v1 surfaces the server's wire verdict through Receive, not Close -
	// CloseResponse is just a "tear down the stream" call. Swallow any
	// *connectv2.Error returned by Close so callers that never invoked
	// Receive don't see a stale verdict. Transport-level (non-Error)
	// failures still propagate so resource issues remain visible.
	if err == nil {
		return nil
	}
	if v1err := errorFromConnectv2(err); v1err != nil {
		// v1 surfaces wire errors through Receive, not Close: see the
		// block comment above.
		return nil //nolint:nilerr
	}
	return wrapIfContextError(err)
}

// flushRequestHeader copies the v1 request header onto the underlying v2
// CallInfo exactly once, immediately before the first stream operation.
func (c *clientStreamConn) flushRequestHeader() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.startedSending || c.info == nil || c.info.v2Info == nil {
		return
	}
	c.startedSending = true
	if c.info.requestHeader != nil {
		copyHTTPHeaderIntoMetadata(c.info.v2Info.RequestHeader(), c.info.requestHeader)
	}
	applyClientDefaultHeaders(c.info.v2Info.RequestHeader(), c.protocol, c.info.requestHeader)
}

// handlerStreamConn projects a v2 [connectv2.ServerStream] onto the v1
// [StreamingHandlerConn] interface. It owns the response header and trailer
// http.Headers so v1 handler code can write into them freely; the wrapper
// copies these into the v2 CallInfo on flush.
type handlerStreamConn struct {
	spec   Spec
	peer   Peer
	stream connectv2.ServerStream
	v2Info *connectv2.CallInfo

	reqHeader  http.Header
	resHeader  http.Header
	resTrailer http.Header

	mu          sync.Mutex
	firstSendOk bool
}

func (c *handlerStreamConn) Spec() Spec                 { return c.spec }
func (c *handlerStreamConn) Peer() Peer                 { return c.peer }
func (c *handlerStreamConn) RequestHeader() http.Header { return c.reqHeader }

func (c *handlerStreamConn) Receive(msg any) error {
	return v1ErrorOnly(c.stream.Receive(msg))
}

func (c *handlerStreamConn) Send(msg any) error {
	c.mu.Lock()
	if !c.firstSendOk {
		c.firstSendOk = true
		// Copy accumulated response header into v2 metadata before the
		// first Send flushes the response. Trailers can still be added
		// after this point and are flushed on close.
		copyHTTPHeaderIntoMetadata(c.v2Info.ResponseHeader(), c.resHeader)
	}
	c.mu.Unlock()
	if msg == nil {
		// v1 streaming handlers treat Send(nil) as a header flush: no
		// envelope is emitted, but response headers are committed and
		// flushed to the client. Start commits the headers now copied into
		// the v2 CallInfo above so clients waiting on response metadata
		// before the first message are unblocked.
		return v1ErrorOnly(c.stream.SendHeaders())
	}
	return v1ErrorOnly(c.stream.Send(msg))
}

func (c *handlerStreamConn) ResponseHeader() http.Header  { return c.resHeader }
func (c *handlerStreamConn) ResponseTrailer() http.Header { return c.resTrailer }

// flushMetadata copies the response header (if not already flushed) and
// trailer into the v2 CallInfo. Called from the handler shim after the user's
// stream function returns.
func (c *handlerStreamConn) flushMetadata() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.firstSendOk {
		copyHTTPHeaderIntoMetadata(c.v2Info.ResponseHeader(), c.resHeader)
		c.firstSendOk = true
	}
	copyHTTPHeaderIntoMetadata(c.v2Info.ResponseTrailer(), c.resTrailer)
}
