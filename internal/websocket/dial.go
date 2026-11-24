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
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// failedBodyLimit and failedBodyTimeout bound what a failed handshake
	// keeps of the response body.
	failedBodyLimit   = 1024
	failedBodyTimeout = 3 * time.Second
)

// DialOptions configures Dial.
type DialOptions struct {
	// HTTPClient makes the handshake request. Its Transport must return a
	// writable body for a 101 response, as [http.Transport] does. Its
	// Timeout bounds only the handshake. Defaults to [http.DefaultClient].
	HTTPClient *http.Client
	// HTTPHeader is added to the handshake request.
	HTTPHeader http.Header
	// Subprotocols lists the subprotocols to offer.
	Subprotocols []string
	// OnPingReceived is called for each ping. Returning false sends no pong.
	OnPingReceived func(context.Context, []byte) bool
}

// Dial opens a WebSocket connection to a ws, wss, http, or https URL. On
// success the response's Body is nil. On failure the response, if any, holds
// up to the first 1024 bytes of its body.
func Dial(ctx context.Context, rawURL string, opts *DialOptions) (*Conn, *http.Response, error) {
	var options DialOptions
	if opts != nil {
		options = *opts
	}
	client := options.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	if client.Timeout > 0 {
		// http.Client's Timeout would also end the connection.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, client.Timeout)
		defer cancel()
		withoutTimeout := *client
		withoutTimeout.Timeout = 0
		client = &withoutTimeout
	}
	request, key, err := newHandshakeRequest(ctx, rawURL, &options)
	if err != nil {
		return nil, nil, fmt.Errorf("dial: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("dial: %w", err)
	}
	body := response.Body
	response.Body = nil
	err = checkUpgradeResponse(response, key, options.Subprotocols)
	transport, ok := body.(io.ReadWriteCloser)
	if err == nil && !ok {
		err = fmt.Errorf("response body %T is not an io.ReadWriteCloser", body)
	}
	if err != nil {
		response.Body = keepFailedBody(response.StatusCode, body)
		return nil, response, fmt.Errorf("dial: %w", err)
	}
	conn := newConn(transport, bufio.NewReader(transport), bufio.NewWriter(transport), true)
	conn.subprotocol = response.Header.Get("Sec-WebSocket-Protocol")
	conn.onPing = options.OnPingReceived
	return conn, response, nil
}

func newHandshakeRequest(ctx context.Context, rawURL string, options *DialOptions) (*http.Request, string, error) {
	requestURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse URL: %w", err)
	}
	switch requestURL.Scheme {
	case "ws":
		requestURL.Scheme = "http"
	case "wss":
		requestURL.Scheme = "https"
	case "http", "https":
	default:
		return nil, "", fmt.Errorf("unsupported URL scheme %q", requestURL.Scheme)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), http.NoBody)
	if err != nil {
		return nil, "", err
	}
	var nonce [16]byte
	_, _ = rand.Read(nonce[:]) // never fails, see crypto/rand.Read
	key := base64.StdEncoding.EncodeToString(nonce[:])
	if options.HTTPHeader != nil {
		request.Header = options.HTTPHeader.Clone()
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", key)
	if len(options.Subprotocols) > 0 {
		request.Header.Set("Sec-WebSocket-Protocol", strings.Join(options.Subprotocols, ","))
	}
	return request, key, nil
}

// checkUpgradeResponse validates the server's handshake, RFC 6455 Section
// 4.2.2. It refuses any extension, since none was offered.
func checkUpgradeResponse(response *http.Response, key string, subprotocols []string) error {
	if response.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("handshake response status is %d, expected 101", response.StatusCode)
	}
	if !headerHasToken(response.Header, "Connection", "upgrade") || !headerHasToken(response.Header, "Upgrade", "websocket") {
		return errors.New("handshake response is not a WebSocket upgrade")
	}
	if accept := response.Header.Get("Sec-WebSocket-Accept"); accept != acceptKey(key) {
		return fmt.Errorf("handshake response has Sec-WebSocket-Accept %q, expected %q", accept, acceptKey(key))
	}
	if extensions := response.Header.Get("Sec-WebSocket-Extensions"); extensions != "" {
		return fmt.Errorf("handshake response has unrequested extensions %q", extensions)
	}
	subprotocol := response.Header.Get("Sec-WebSocket-Protocol")
	if subprotocol == "" {
		return nil
	}
	for _, offered := range subprotocols {
		if strings.EqualFold(offered, subprotocol) {
			return nil
		}
	}
	return fmt.Errorf("handshake response has unoffered subprotocol %q", subprotocol)
}

// keepFailedBody closes body after reading its start, for error messages.
// The body of a 101 is the connection, with nothing to read.
func keepFailedBody(status int, body io.ReadCloser) io.ReadCloser {
	if status == http.StatusSwitchingProtocols {
		_ = body.Close()
		return http.NoBody
	}
	timer := time.AfterFunc(failedBodyTimeout, func() { _ = body.Close() })
	defer timer.Stop()
	data, _ := io.ReadAll(io.LimitReader(body, failedBodyLimit))
	_ = body.Close()
	return io.NopCloser(bytes.NewReader(data))
}
