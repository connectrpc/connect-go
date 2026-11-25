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
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 Section 4.2.2 requires SHA-1
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// acceptGUID is from RFC 6455 Section 1.3.
const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Accept upgrades an HTTP/1.1 request. It refuses a request whose Origin host
// differs from its Host with 403. It hijacks the connection and keeps any
// bytes net/http had already buffered. On failure it writes an HTTP error.
func Accept(responseWriter http.ResponseWriter, request *http.Request, opts Options) (*Conn, error) {
	if status, err := checkUpgradeRequest(responseWriter.Header(), request); err != nil {
		http.Error(responseWriter, err.Error(), status)
		return nil, fmt.Errorf("websocket: accept: %w", err)
	}
	if err := checkOrigin(request); err != nil {
		http.Error(responseWriter, err.Error(), http.StatusForbidden)
		return nil, fmt.Errorf("websocket: accept: %w", err)
	}
	hijacker, ok := findHijacker(responseWriter)
	if !ok {
		http.Error(responseWriter, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
		return nil, errors.New("websocket: accept: response writer can't be hijacked")
	}
	header := responseWriter.Header()
	header.Set("Upgrade", "websocket")
	header.Set("Connection", "Upgrade")
	header.Set("Sec-WebSocket-Accept", acceptKey(request.Header.Get("Sec-WebSocket-Key")))
	if opts.Subprotocol != "" && slices.Contains(headerTokens(request.Header, "Sec-WebSocket-Protocol"), opts.Subprotocol) {
		header.Set("Sec-WebSocket-Protocol", opts.Subprotocol)
	}
	deflate := false
	if opts.Compression {
		var extensions string
		if extensions, deflate = negotiateDeflate(request.Header.Values("Sec-WebSocket-Extensions")); deflate {
			header.Set("Sec-WebSocket-Extensions", extensions)
		}
	}
	responseWriter.WriteHeader(http.StatusSwitchingProtocols)
	netConn, readWriter, err := hijacker.Hijack()
	if err != nil {
		http.Error(responseWriter, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return nil, fmt.Errorf("websocket: accept: hijack: %w", err)
	}
	// Reading past the buffered bytes of a hijacked reader fails, see
	// golang/go#32314. Read the connection directly and keep any buffered
	// bytes in front of it.
	if buffered := readWriter.Reader.Buffered(); buffered > 0 {
		data, _ := readWriter.Peek(buffered)
		readWriter.Reader.Reset(io.MultiReader(bytes.NewReader(data), netConn))
	} else {
		readWriter.Reader.Reset(netConn)
	}
	return newDeflateConn(netConn, readWriter.Reader, false, deflate, opts), nil
}

// Dial runs the client handshake for request, a GET to an http or https URL.
// It sets the Upgrade, Connection and Sec-WebSocket-* headers. The request's
// context bounds the handshake. An [*http.Client]'s Timeout bounds only the
// handshake and never ends the connection itself. On success the 101 body
// becomes the transport and the response's Body is [http.NoBody]. On any other
// status Dial returns the response with an error, and the caller reads and
// closes Body.
func Dial(httpClient HTTPClient, request *http.Request, opts Options) (*Conn, *http.Response, error) {
	if request.Method != "" && request.Method != http.MethodGet {
		return nil, nil, fmt.Errorf("websocket: dial: method is %q, expected GET", request.Method)
	}
	if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
		return nil, nil, fmt.Errorf("websocket: dial: unsupported URL scheme %q", request.URL.Scheme)
	}
	ctx := request.Context()
	if client, ok := httpClient.(*http.Client); ok && client.Timeout > 0 {
		// http.Client's Timeout would also end the connection.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, client.Timeout)
		defer cancel()
		withoutTimeout := *client
		withoutTimeout.Timeout = 0
		httpClient = &withoutTimeout
	}
	request = request.Clone(ctx)
	var nonce [16]byte
	_, _ = rand.Read(nonce[:]) // never fails, see crypto/rand.Read
	key := base64.StdEncoding.EncodeToString(nonce[:])
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", key)
	if opts.Compression {
		request.Header.Set("Sec-WebSocket-Extensions", deflateOffer)
	} else {
		request.Header.Del("Sec-WebSocket-Extensions")
	}
	if opts.Subprotocol != "" {
		request.Header.Set("Sec-WebSocket-Protocol", opts.Subprotocol)
	} else {
		request.Header.Del("Sec-WebSocket-Protocol")
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("websocket: dial: %w", err)
	}
	if response == nil {
		return nil, nil, errors.New("websocket: dial: HTTP client returned no response")
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		return nil, response, fmt.Errorf("websocket: dial: handshake response status is %d, expected 101", response.StatusCode)
	}
	body := response.Body
	response.Body = http.NoBody
	deflate, err := checkUpgradeResponse(response, key, opts)
	transport, ok := body.(io.ReadWriteCloser)
	if err == nil && !ok {
		err = fmt.Errorf("response body %T is not an io.ReadWriteCloser", body)
	}
	if err != nil {
		if body != nil {
			_ = body.Close()
		}
		return nil, response, fmt.Errorf("websocket: dial: %w", err)
	}
	return newDeflateConn(transport, bufio.NewReader(transport), true, deflate, opts), response, nil
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

// checkUpgradeResponse validates the server's handshake, RFC 6455 Section
// 4.2.2, and reports whether it enabled compression. It refuses any
// extension that wasn't offered.
func checkUpgradeResponse(response *http.Response, key string, opts Options) (bool, error) {
	if !headerHasToken(response.Header, "Connection", "upgrade") || !headerHasToken(response.Header, "Upgrade", "websocket") {
		return false, errors.New("handshake response is not a WebSocket upgrade")
	}
	if accept := response.Header.Get("Sec-WebSocket-Accept"); accept != acceptKey(key) {
		return false, fmt.Errorf("handshake response has Sec-WebSocket-Accept %q, expected %q", accept, acceptKey(key))
	}
	extensions := response.Header.Values("Sec-WebSocket-Extensions")
	deflate := false
	if opts.Compression {
		var err error
		if deflate, err = verifyDeflateResponse(extensions); err != nil {
			return false, err
		}
	} else if joined := strings.Join(extensions, ""); strings.TrimSpace(joined) != "" {
		return false, fmt.Errorf("handshake response has unrequested extensions %q", strings.Join(extensions, ", "))
	}
	if got := response.Header.Get("Sec-WebSocket-Protocol"); got != opts.Subprotocol {
		return false, fmt.Errorf("handshake response has subprotocol %q, expected %q", got, opts.Subprotocol)
	}
	return deflate, nil
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
