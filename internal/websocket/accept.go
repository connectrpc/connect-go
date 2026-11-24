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

package websocket

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // RFC 6455 Section 4.2.2 requires SHA-1
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// acceptGUID is from RFC 6455 Section 1.3.
const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// AcceptOptions configures Accept.
type AcceptOptions struct {
	// Subprotocols lists the subprotocols the server supports, in order of
	// preference. The handshake succeeds without one if the client offers
	// none of them.
	Subprotocols []string
}

// Accept upgrades an HTTP/1.1 request to a WebSocket connection. It refuses
// a cross-origin request, one whose Origin host differs from the request's
// Host. On failure it writes an HTTP error response.
func Accept(responseWriter http.ResponseWriter, request *http.Request, opts *AcceptOptions) (*Conn, error) {
	if status, err := checkUpgradeRequest(responseWriter.Header(), request); err != nil {
		http.Error(responseWriter, err.Error(), status)
		return nil, fmt.Errorf("accept: %w", err)
	}
	if err := checkOrigin(request); err != nil {
		http.Error(responseWriter, err.Error(), http.StatusForbidden)
		return nil, fmt.Errorf("accept: %w", err)
	}
	hijacker, ok := findHijacker(responseWriter)
	if !ok {
		http.Error(responseWriter, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
		return nil, errors.New("accept: response writer can't be hijacked")
	}
	header := responseWriter.Header()
	header.Set("Upgrade", "websocket")
	header.Set("Connection", "Upgrade")
	header.Set("Sec-WebSocket-Accept", acceptKey(request.Header.Get("Sec-WebSocket-Key")))
	if opts != nil {
		if subprotocol := selectSubprotocol(request.Header, opts.Subprotocols); subprotocol != "" {
			header.Set("Sec-WebSocket-Protocol", subprotocol)
		}
	}
	responseWriter.WriteHeader(http.StatusSwitchingProtocols)
	netConn, readWriter, err := hijacker.Hijack()
	if err != nil {
		http.Error(responseWriter, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return nil, fmt.Errorf("accept: hijack: %w", err)
	}
	// Reading past the buffered bytes of a hijacked reader fails, see
	// golang/go#32314. Keep them in front of the connection instead.
	buffered, _ := readWriter.Peek(readWriter.Reader.Buffered())
	readWriter.Reader.Reset(io.MultiReader(bytes.NewReader(buffered), netConn))
	conn := newConn(netConn, readWriter.Reader, readWriter.Writer, false)
	conn.subprotocol = header.Get("Sec-WebSocket-Protocol")
	return conn, nil
}

// checkUpgradeRequest validates the client's handshake, RFC 6455 Section
// 4.2.1. It returns the HTTP status to refuse with.
func checkUpgradeRequest(responseHeader http.Header, request *http.Request) (int, error) {
	if !request.ProtoAtLeast(1, 1) {
		return http.StatusUpgradeRequired, fmt.Errorf("handshake needs at least HTTP/1.1, got %q", request.Proto)
	}
	if !headerHasToken(request.Header, "Connection", "upgrade") || !headerHasToken(request.Header, "Upgrade", "websocket") {
		responseHeader.Set("Connection", "Upgrade")
		responseHeader.Set("Upgrade", "websocket")
		return http.StatusUpgradeRequired, errors.New("handshake is not a WebSocket upgrade")
	}
	if request.Method != http.MethodGet {
		return http.StatusMethodNotAllowed, fmt.Errorf("handshake method is %q, expected GET", request.Method)
	}
	if version := request.Header.Get("Sec-WebSocket-Version"); version != "13" {
		responseHeader.Set("Sec-WebSocket-Version", "13")
		return http.StatusBadRequest, fmt.Errorf("unsupported WebSocket version %q, expected 13", version)
	}
	keys := request.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return http.StatusBadRequest, fmt.Errorf("handshake has %d Sec-WebSocket-Key headers, expected 1", len(keys))
	}
	if key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keys[0])); err != nil || len(key) != 16 {
		return http.StatusBadRequest, fmt.Errorf("Sec-WebSocket-Key %q is not 16 bytes of base64", keys[0])
	}
	return 0, nil
}

// checkOrigin refuses cross-site WebSocket hijacking. Browsers always send
// Origin, other clients rarely do.
func checkOrigin(request *http.Request) error {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	originURL, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("parse Origin header %q: %w", origin, err)
	}
	if strings.EqualFold(originURL.Host, request.Host) {
		return nil
	}
	if originURL.Host == "" {
		return fmt.Errorf("origin %q has no host", origin)
	}
	return fmt.Errorf("origin %q is not authorized for host %q", originURL.Host, request.Host)
}

// findHijacker follows Unwrap like [http.ResponseController], but before
// anything is written.
func findHijacker(responseWriter http.ResponseWriter) (http.Hijacker, bool) {
	for {
		switch writer := responseWriter.(type) {
		case http.Hijacker:
			return writer, true
		case interface{ Unwrap() http.ResponseWriter }:
			responseWriter = writer.Unwrap()
		default:
			return nil, false
		}
	}
}

// selectSubprotocol returns the client's spelling of the first supported
// subprotocol it offered.
func selectSubprotocol(requestHeader http.Header, supported []string) string {
	offered := headerTokens(requestHeader, "Sec-WebSocket-Protocol")
	for _, subprotocol := range supported {
		for _, offer := range offered {
			if strings.EqualFold(subprotocol, offer) {
				return offer
			}
		}
	}
	return ""
}

func acceptKey(key string) string {
	hash := sha1.Sum([]byte(key + acceptGUID)) //nolint:gosec // RFC 6455 Section 4.2.2 requires SHA-1
	return base64.StdEncoding.EncodeToString(hash[:])
}

func headerHasToken(header http.Header, key, token string) bool {
	for _, value := range headerTokens(header, key) {
		if strings.EqualFold(value, token) {
			return true
		}
	}
	return false
}

func headerTokens(header http.Header, key string) []string {
	var tokens []string
	for _, value := range header.Values(key) {
		for token := range strings.SplitSeq(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				tokens = append(tokens, token)
			}
		}
	}
	return tokens
}
