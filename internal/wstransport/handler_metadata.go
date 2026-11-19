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

package wstransport

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect/v2"
)

// errNoOpeningMessage is the cause when a client's opening M does not arrive
// within the handshake timeout.
var errNoOpeningMessage = errors.New("no opening M message")

// drainLeadingMetadata consumes the single M message that opens
// every stream, so CallInfo is complete and frozen before the handler exists.
// Without it the merge happens on the read path, under the handler, and
// anything the handler does concurrently with its first Receive races that
// write.
//
// Exactly one message, not one-or-more: a drain that kept reading while frames
// were metadata could only discover the end by blocking for a frame the client
// may never send. A receive-only client sends its opening metadata and then
// waits for the server, so the drain must know it is done after one frame.
func (c *websocketHandlerConn) drainLeadingMetadata(ctx context.Context) *connect.Error {
	c.fault = faultNone
	wire, readErr := c.nextOpeningMessage(ctx)
	if readErr != nil {
		c.reportFault(readErr)
		return readErr
	}
	if wire.marker != markerMetadata {
		// Anything before the opening metadata means the peer is not speaking
		// this protocol; there is no partial state worth keeping.
		failure := c.fail(
			FaultMetadata,
			"client's first message must be M; got %s", markerName(wire.marker),
		)
		c.reportFault(failure)
		return failure
	}
	// §6.1: M is always JSON, and a binary frame says its payload is not.
	if !wire.text {
		failure := c.fail(FaultFrameType, "client sent M in a binary frame; expected a text frame")
		c.reportFault(failure)
		return failure
	}
	mergeErr := c.mergeLeadingMetadata(wire.payload)
	if mergeErr != nil {
		c.reportFault(mergeErr)
	}
	return mergeErr
}

// nextOpeningMessage reads the client's first message, which must arrive
// within the handshake timeout rather than the RPC's much longer deadline.
func (c *websocketHandlerConn) nextOpeningMessage(callCtx context.Context) (wireMessage, *connect.Error) {
	timeout := c.info.HandshakeTimeout
	if timeout <= 0 {
		return c.nextMessage(callCtx)
	}
	openingCtx, cancel := context.WithTimeoutCause(callCtx, timeout, errNoOpeningMessage)
	defer cancel()
	wire, readErr := c.nextMessage(openingCtx)
	if readErr != nil && callCtx.Err() == nil && errors.Is(context.Cause(openingCtx), errNoOpeningMessage) {
		return wire, errorf(
			connect.CodeDeadlineExceeded, "client sent no opening M message within %v", timeout,
		)
	}
	return wire, readErr
}

func (c *websocketHandlerConn) mergeLeadingMetadata(payload []byte) *connect.Error {
	entries, parseErr := c.metadataEntries(payload)
	if parseErr != nil {
		return parseErr
	}
	// Checked against the keys as sent, before canonicalization, so the error
	// names the spelling the client used rather than an internal form of it.
	// A reserved key ends the RPC rather than being dropped: a client that
	// believed it had set an identity header would otherwise never learn that
	// the server does not have it.
	for _, entry := range entries {
		if len(entry) == 0 {
			continue // decodeMetadata rejects it
		}
		if reason, reserved := reservedHeaderReason(entry[0], c.info.InfrastructureHeaders); reserved {
			return c.fail(
				FaultMetadata,
				"client set %q in its M message, which is %s", entry[0], reason,
			)
		}
	}
	meta, decodeErr := c.metadataHeader(entries)
	if decodeErr != nil {
		return decodeErr
	}
	// An M key replaces the upgrade request's value, producing the effective
	// headers the handler and its interceptors see. What the connection itself
	// established is protected by the reserved list above, not by precedence.
	header := c.callInfo.RequestHeader()
	for key, values := range meta {
		header.SetValues(key, values)
	}
	return nil
}

// flushLeadingMetadata writes the server's M message. It must run before the
// first body and before the S message alike: a stream that fails without
// sending a body is where leading metadata is most wanted, and there would be
// nothing else to carry it.
//
// It goes out even when there is no metadata, as []. Every stream opens with
// exactly one M in each direction, so a client can read its first message
// without a branch for the empty case — and can take that message as the
// signal that the server has accepted the stream, which is what the response
// header block gives it for free over HTTP.
func (c *websocketHandlerConn) flushLeadingMetadata(ctx context.Context) *connect.Error {
	if c.leadingSent {
		return nil
	}
	header := make(http.Header)
	// Read at flush time rather than copied in: a handler sets its leading
	// metadata during the call, so no earlier snapshot would be current.
	if callInfo := c.callInfo; callInfo != nil {
		for key, values := range callInfo.ResponseHeader().All() {
			// §7.3.2: a server never sends a connect- key in its M.
			if isConnectReserved(strings.ToLower(key)) {
				continue
			}
			header[http.CanonicalHeaderKey(key)] = values
		}
	}
	entries, encodeErr := encodeMetadata(header)
	if encodeErr != nil {
		// The handler set a header this server cannot send (§7.3.1). M still
		// opens the stream, empty, and the header becomes the RPC's outcome.
		entries = [][]string{}
		c.headerErr = encodeErr
	}
	// Marked only once written, so that Close still opens with M after a
	// failed Send.
	if err := c.marshaler.writeJSON(ctx, markerMetadata, entries); err != nil {
		return err
	}
	c.leadingSent = true
	return c.headerErr
}

// responseTrailer is what the handler set as trailing metadata. It is read here
// because Close runs once the handler has returned, when it is final.
func (c *websocketHandlerConn) responseTrailer() http.Header {
	trailer := make(http.Header)
	if callInfo := c.callInfo; callInfo != nil {
		copyToHTTPHeader(trailer, callInfo.ResponseTrailer())
	}
	return trailer
}
