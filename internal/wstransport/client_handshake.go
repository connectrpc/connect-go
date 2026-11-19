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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/connectwire"
	"github.com/coder/websocket"
)

// handshake dials and checks what the server agreed to. A connection it
// rejects is closed.
func (c *wsClientCall) handshake(ctx context.Context) (*websocket.Conn, *connect.Error) {
	// The handshake gets its own deadline; the connection outlives it.
	dialCtx := ctx
	if c.config.handshakeTimeout > 0 {
		var cancelDial context.CancelFunc
		dialCtx, cancelDial = context.WithTimeout(ctx, c.config.handshakeTimeout)
		defer cancelDial()
	}
	conn, response, err := websocket.Dial(dialCtx, c.url.String(), c.dialOptions)
	c.response = response
	var failedBody []byte
	if response != nil && response.Body != nil {
		// We only need the upgrade response's headers; close the body to
		// avoid leaking it. On a failed handshake the library has replaced
		// it with the first KiB, which may hold a Connect error.
		if err != nil {
			failedBody, _ = io.ReadAll(response.Body)
		}
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, dialError(err, response, failedBody)
	}
	// The library has already failed a handshake that echoed a token this
	// client did not offer (RFC 6455 §4.1). §4.3 fails one that echoed none
	// the same way: the handshake failed, so no Close frame is sent.
	token := c.config.selectedSubprotocol(conn.Subprotocol())
	if token == "" {
		_ = conn.CloseNow()
		return nil, errorf(
			connect.CodeInternal,
			"server echoed no Sec-WebSocket-Protocol (offered %s)",
			strings.Join(c.config.offered, ", "),
		)
	}
	// Every offered token names a codec this client holds.
	if codec, ok := c.config.codecs.get(codecForSubprotocol(token)); ok {
		c.codec = codec
	}
	if takeoverErr := checkNoContextTakeover(response); takeoverErr != nil {
		_ = closeClientWithError(
			conn, c.drain,
			"permessage-deflate requires client_no_context_takeover and server_no_context_takeover",
		)
		return nil, takeoverErr
	}
	return conn, nil
}

// dialError turns a failed handshake into the RPC's error. §4.5: a Connect
// error in the response body wins, then Connect-over-HTTP's status mapping. A
// 101 that still failed broke the handshake's own rules, such as echoing a
// token this client did not offer, which is the server's fault: Internal.
func dialError(err error, response *http.Response, body []byte) *connect.Error {
	switch {
	case errors.Is(err, context.Canceled):
		return errorf(connect.CodeCanceled, "WebSocket upgrade canceled: %w", err)
	case errors.Is(err, context.DeadlineExceeded):
		return errorf(connect.CodeDeadlineExceeded, "WebSocket upgrade deadline exceeded: %w", err)
	}
	// Callers read io.EOF as a clean end of stream, so a connection dropped
	// mid-handshake is reported as unexpected, as connecthttp does.
	if errors.Is(err, io.EOF) {
		err = fmt.Errorf("%s: %w", err.Error(), io.ErrUnexpectedEOF)
	}
	if response == nil {
		return errorf(connect.CodeUnavailable, "WebSocket upgrade failed: %w", err)
	}
	if response.StatusCode == http.StatusSwitchingProtocols {
		return errorf(connect.CodeInternal, "WebSocket upgrade failed: %w", err)
	}
	if wireErr := handshakeWireError(response, body); wireErr != nil {
		return wireErr
	}
	return errorf(
		httpToCode(response.StatusCode),
		"WebSocket upgrade failed: HTTP %s: %w",
		response.Status, err,
	)
}

// handshakeWireError reads a Connect error from a rejected handshake's body,
// or returns nil if the body is not one. A body without a recognizable code
// is not one either, so the status mapping still applies to it.
func handshakeWireError(response *http.Response, body []byte) *connect.Error {
	mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if !strings.EqualFold(mediaType, "application/json") || len(body) == 0 {
		return nil
	}
	var wireErr connectwire.WireError
	if json.Unmarshal(body, &wireErr) == nil && wireErr.Code != 0 {
		return wireErr.AsError()
	}
	return nil
}

// httpToCode maps an HTTP status to a Connect code. Copied from connecthttp,
// where it is unexported.
//
// https://github.com/grpc/grpc/blob/master/doc/http-grpc-status-mapping.md
// Note that this is NOT the inverse of the gRPC-to-HTTP or Connect-to-HTTP
// mappings.
func httpToCode(httpCode int) connect.Code {
	// Literals are easier to compare to the specification (vs named
	// constants).
	switch httpCode {
	case 400:
		return connect.CodeInternal
	case 401:
		return connect.CodeUnauthenticated
	case 403:
		return connect.CodePermissionDenied
	case 404:
		return connect.CodeUnimplemented
	case 429:
		return connect.CodeUnavailable
	case 502, 503, 504:
		return connect.CodeUnavailable
	default:
		return connect.CodeUnknown
	}
}

// checkNoContextTakeover refuses a handshake that negotiated
// permessage-deflate with a compression context shared across messages.
//
// A shared context leaks plaintext between messages whenever attacker-
// influenced and secret data travel on one connection — the CRIME/BREACH
// family. The server is required to impose no-context-takeover, but a client
// that trusts it to have done so is protected only as far as the peer is
// conforming, and the client's own plaintext is what leaks.
//
// No extension at all is fine: nothing is compressed, so nothing leaks.
func checkNoContextTakeover(response *http.Response) *connect.Error {
	if response == nil {
		return nil
	}
	for _, value := range response.Header.Values(wsHeaderExtensions) {
		for extension := range strings.SplitSeq(value, ",") {
			parameters := strings.Split(extension, ";")
			if strings.TrimSpace(parameters[0]) != "permessage-deflate" {
				continue
			}
			var client, server bool
			for _, parameter := range parameters[1:] {
				switch strings.TrimSpace(parameter) {
				case "client_no_context_takeover":
					client = true
				case "server_no_context_takeover":
					server = true
				}
			}
			if !client || !server {
				return errorf(
					connect.CodeInternal,
					"server negotiated %q without no-context-takeover in both directions; "+
						"a shared compression context leaks plaintext across messages",
					strings.TrimSpace(extension),
				)
			}
		}
	}
	return nil
}

// closeClientWithError ends a client's connection with 3102. The code alone
// cannot tell a server what went wrong, so §13 requires the reason.
func closeClientWithError(conn *websocket.Conn, limiter *drainLimiter, reason string) error {
	return closeWithDrain(conn, limiter, wsStatusClientProtocolError, closeReason(reason))
}
