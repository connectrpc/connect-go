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

// Package websocket implements the parts of RFC 6455 that Connect over
// WebSocket needs. It reads whole messages, writes each message as one frame
// and runs the close handshake. It starts no goroutines or timers and takes no
// contexts. Closing the Conn is the only way to interrupt a read or write.
package websocket

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// MaxHeaderSize is the space WriteMessage needs in front of the payload. It
// fits the longest frame header, with a 64-bit length and a mask key.
const MaxHeaderSize = 14

const (
	// MessageText is UTF-8 text. ReadMessage validates it as it arrives.
	MessageText MessageType = 1
	// MessageBinary is binary data.
	MessageBinary MessageType = 2
)

// StatusCode constants are from RFC 6455 Section 7.4.1.
const (
	StatusNormalClosure StatusCode = 1000
	StatusGoingAway     StatusCode = 1001
	// StatusProtocolError is sent by ReadMessage with ErrProtocol.
	StatusProtocolError StatusCode = 1002
	// StatusNoStatusReceived is reported for a Close frame without a code.
	// It never goes on the wire. WriteClose sends it as a Close frame without
	// a code.
	StatusNoStatusReceived StatusCode = 1005
	// StatusInvalidPayloadData is sent by ReadMessage with ErrInvalidUTF8.
	StatusInvalidPayloadData StatusCode = 1007
	// StatusMessageTooBig is sent by ReadMessage with ErrMessageTooBig.
	StatusMessageTooBig StatusCode = 1009
	StatusInternalError StatusCode = 1011
)

// maxCloseReason leaves room for the code in a control frame.
const maxCloseReason = maxControlPayload - 2

var (
	// ErrProtocol is wrapped by ReadMessage's error when the peer broke RFC
	// 6455 framing. The Conn has already sent a Close frame with 1002.
	ErrProtocol = errors.New("websocket: protocol error")
	// ErrInvalidUTF8 is wrapped by ReadMessage's error for a text message that
	// isn't valid UTF-8. The Conn has already sent a Close frame with 1007.
	ErrInvalidUTF8 = errors.New("websocket: invalid UTF-8 in text message")
	// ErrMessageTooBig is wrapped by ReadMessage's error for a message over its
	// limit. The Conn read none of the oversized frame's payload and has
	// already sent a Close frame with 1009.
	ErrMessageTooBig = errors.New("websocket: message too big")
)

// MessageType is the type of a data message.
type MessageType byte

// StatusCode is a Close frame's status code. Applications may use 3000-4999.
type StatusCode uint16

// CloseError is the peer's Close frame. ReadMessage returns it after the close
// handshake completes and the transport is closed.
type CloseError struct {
	Code   StatusCode
	Reason string
}

// Error reports the code and reason.
func (e *CloseError) Error() string {
	return fmt.Sprintf("websocket: peer closed with %d %q", e.Code, e.Reason)
}

// HTTPClient makes the handshake request. [*http.Client] implements it. The
// response to a successful handshake must keep its 101 body, which must be an
// [io.ReadWriteCloser], as [http.Transport] returns it.
type HTTPClient interface {
	Do(request *http.Request) (*http.Response, error)
}

// Options configures Accept and Dial. The zero value offers and accepts no
// subprotocol and no extension.
type Options struct {
	// Subprotocol is offered by Dial and echoed by Accept when the client
	// offered it. Dial fails unless the server echoes it exactly.
	Subprotocol string
}

// validCloseCode reports whether code may appear in a Close frame.
func validCloseCode(code StatusCode) bool {
	switch {
	case code == 1004, code == StatusNoStatusReceived, code == 1006:
		return false
	case code >= StatusNormalClosure && code <= 1014:
		return true
	default:
		return code >= 3000 && code <= 4999
	}
}

// closeReason fits text into a Close frame as valid UTF-8, cutting it on a
// rune boundary.
func closeReason(text string) string {
	if len(text) > maxCloseReason {
		text = text[:maxCloseReason]
	}
	if utf8.ValidString(text) {
		return text
	}
	return strings.ToValidUTF8(text, "")
}
