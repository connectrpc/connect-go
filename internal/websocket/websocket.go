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
// WebSocket needs. Its API is a subset of github.com/coder/websocket with
// the same semantics. It doesn't support extensions, and each Write sends one
// unfragmented frame.
package websocket

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// MessageType constants follow the data frame opcodes.
const (
	// MessageText is a message of UTF-8 text.
	MessageText MessageType = MessageType(opText)
	// MessageBinary is a message of binary data.
	MessageBinary MessageType = MessageType(opBinary)
)

// StatusCode constants are from RFC 6455 Section 7.4.1. Applications may use
// 3000-4999.
const (
	StatusNormalClosure   StatusCode = 1000
	StatusGoingAway       StatusCode = 1001
	StatusProtocolError   StatusCode = 1002
	StatusUnsupportedData StatusCode = 1003
	// StatusNoStatusRcvd is reported for a Close frame without a code. It is
	// never sent.
	StatusNoStatusRcvd StatusCode = 1005
	// StatusAbnormalClosure is never sent or reported. A connection that ends
	// without a Close frame returns an error without a CloseError.
	StatusAbnormalClosure         StatusCode = 1006
	StatusInvalidFramePayloadData StatusCode = 1007
	StatusPolicyViolation         StatusCode = 1008
	StatusMessageTooBig           StatusCode = 1009
	StatusMandatoryExtension      StatusCode = 1010
	StatusInternalError           StatusCode = 1011
)

const (
	defaultReadLimit = 32768
	// closeTimeout bounds writing a Close frame and waiting for the peer's.
	closeTimeout = 5 * time.Second
	// maxCloseReason leaves room for the code in a control frame.
	maxCloseReason = maxControlPayload - 2
)

var (
	// ErrMessageTooBig is wrapped by the error a read returns for a message
	// over the read limit.
	ErrMessageTooBig = errors.New("websocket: message too big")
	// ErrInvalidUTF8 is wrapped by the error a read returns for a text message
	// that isn't valid UTF-8.
	ErrInvalidUTF8 = errors.New("websocket: invalid UTF-8 in text message")
)

// MessageType is the type of a data message.
type MessageType int

// StatusCode is a Close frame's status code.
type StatusCode int

// CloseError is a Close frame's code and reason. Reads return an error
// wrapping it after the peer closes. When a read fails the connection itself,
// its error doesn't wrap the CloseError it sent, as in coder/websocket.
type CloseError struct {
	Code   StatusCode
	Reason string
}

func (e CloseError) Error() string {
	return fmt.Sprintf("status = %d and reason = %q", e.Code, e.Reason)
}

// CloseStatus returns the code of the CloseError that err wraps, or -1.
func CloseStatus(err error) StatusCode {
	if closeErr, ok := errors.AsType[CloseError](err); ok {
		return closeErr.Code
	}
	return -1
}

// validCloseCode reports whether code may appear in a Close frame.
func validCloseCode(code StatusCode) bool {
	switch {
	case code == 1004, code == StatusNoStatusRcvd, code == StatusAbnormalClosure:
		return false
	case code >= StatusNormalClosure && code <= 1014:
		return true
	default:
		return code >= 3000 && code <= 4999
	}
}

// closeReason fits text into a Close frame.
func closeReason(text string) string {
	if len(text) <= maxCloseReason {
		return text
	}
	return strings.ToValidUTF8(text[:maxCloseReason], "")
}
