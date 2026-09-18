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

package connectwebsocket

import (
	"fmt"

	"connectrpc.com/connect/v2"
)

// errorf returns a [connect.Error] with a formatted message, attaching
// anything wrapped with %w as the local cause.
//
// [connect.Errorf] formats with fmt.Sprintf, which renders %w as %!w(...) and
// drops the cause. The framing code relies on errors.Is reaching io.EOF and
// context cancellation through the returned error, so it needs fmt.Errorf
// semantics plus [connect.Error.WithCause].
func errorf(code connect.Code, format string, args ...any) *connect.Error {
	formatted := fmt.Errorf(format, args...)
	connectErr := connect.NewError(code, formatted.Error())
	if wraps(formatted) {
		// Attach the whole chain, so multiple %w verbs survive too.
		return connectErr.WithCause(formatted)
	}
	return connectErr
}

// wraps reports whether err carries a cause that errors.Is should reach.
func wraps(err error) bool {
	switch err.(type) { //nolint:errorlint // asking about err itself, not its chain
	case interface{ Unwrap() error }, interface{ Unwrap() []error }:
		return true
	}
	return false
}
