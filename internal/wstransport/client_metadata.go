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
	"maps"
	"net/http"

	"connectrpc.com/connect/v2"
)

// requestMetadata reads the caller's request headers as they stand when M is
// written, which is the first Send as it is over HTTP.
//
// They travel in M and nowhere else. Copying them onto the handshake too would
// put the same keys on two channels, and the client's own M would be shadowed
// by its own headers the moment the two disagreed.
func (c *websocketClientConn) requestMetadata() http.Header {
	header := make(http.Header, 8)
	if c.callInfo == nil {
		return header
	}
	for key, values := range c.callInfo.RequestHeader().All() {
		header[http.CanonicalHeaderKey(key)] = append(header[http.CanonicalHeaderKey(key)], values...)
	}
	return header
}

// ResponseHeader returns a copy of the response headers, safe to read while
// Receive runs.
func (c *websocketClientConn) ResponseHeader() http.Header {
	// Dialed before locking: the handshake can take as long as its timeout, and
	// Receive and ResponseTrailer would wait behind it.
	dialErr := c.call.ensureDialed(c.ctx)
	c.metadataMu.Lock()
	defer c.metadataMu.Unlock()
	// Leading-Metadata replaces same-key handshake values rather than appending
	// to them, so this runs after the 101's headers and assigns rather than
	// merging — which also keeps a second call from duplicating them.
	c.responseHeaderOnce.Do(func() {
		if dialErr != nil {
			return
		}
		for key, values := range c.call.handshakeHeader() {
			if isWebSocketProtocolHeader(key) {
				continue
			}
			c.responseHeader[key] = values
		}
	})
	maps.Copy(c.responseHeader, c.header)
	return c.responseHeader.Clone()
}

// ResponseTrailer returns a copy of the response trailers, safe to read while
// Receive runs.
func (c *websocketClientConn) ResponseTrailer() http.Header {
	c.metadataMu.Lock()
	defer c.metadataMu.Unlock()
	return c.responseTrailer.Clone()
}

// mergeLeadingMetadata folds the server's M message into the response headers.
//
// Exactly one arrives, and it opens the response stream. A second would mean
// the server is rewriting headers the application may already have read, which
// is the same race the request side refuses.
func (c *websocketClientConn) mergeLeadingMetadata(payload []byte) *connect.Error {
	if c.sawData {
		return c.fail(
			FaultMetadata,
			"server sent M after a body; metadata is leading only before the first one",
		)
	}
	if c.sawLeadingMetadata {
		return c.fail(
			FaultMetadata,
			"server sent a second M message; a stream carries exactly one, and it opens the stream",
		)
	}
	c.sawLeadingMetadata = true
	entries, parseErr := c.metadataEntries(payload)
	if parseErr != nil {
		return parseErr
	}
	meta, decodeErr := c.metadataHeader(entries)
	if decodeErr != nil {
		return decodeErr
	}
	c.metadataMu.Lock()
	defer c.metadataMu.Unlock()
	if c.header == nil {
		c.header = make(http.Header, len(meta))
	}
	maps.Copy(c.header, meta)
	return nil
}

// publishMetadata copies the response metadata onto the call's
// [connect.CallInfo], where callers read it. It runs once.
//
// The write happens during Receive, so a caller reading CallInfo from another
// goroutine while receiving on the same stream races it. That is not specific
// to this transport — connecthttp writes the same object from inside its own
// Receive — so it is left to be fixed where connect.Header is defined rather
// than papered over here; see "Current limitations" in
// docs/websocket-transport-design.md.
func (c *websocketClientConn) publishMetadata() {
	c.publishOnce.Do(func() {
		if c.callInfo == nil {
			return
		}
		c.publishHeaders()
		for key, values := range c.ResponseTrailer() {
			c.callInfo.ResponseTrailer().SetValues(key, values)
		}
		// Identity at the Connect layer: permessage-deflate is below the protocol.
		c.callInfo.ResponseEncoding = connect.CompressionNameIdentity
	})
}

// publishHeaders copies the response headers onto the call's
// [connect.CallInfo]. It runs once.
func (c *websocketClientConn) publishHeaders() {
	c.publishHeadersOnce.Do(func() {
		if c.callInfo == nil {
			return
		}
		for key, values := range c.ResponseHeader() {
			c.callInfo.ResponseHeader().SetValues(key, values)
		}
	})
}

func isWebSocketProtocolHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case wsHeaderUpgrade,
		wsHeaderConnection,
		"Sec-Websocket-Accept",
		wsHeaderProtocol,
		wsHeaderExtensions,
		"Sec-Websocket-Version",
		"Sec-Websocket-Key":
		return true
	}
	return false
}
