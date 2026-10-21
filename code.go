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
	connectv2 "connectrpc.com/connect/v2"
)

// A Code is one of the Connect protocol's error codes. There are no user-defined
// codes, so only the codes enumerated below are valid. In both name and
// semantics, these codes match the gRPC status codes.
//
// The descriptions below are optimized for brevity rather than completeness.
// See the [Connect protocol specification] for detailed descriptions of each
// code and example usage.
//
// [Connect protocol specification]: https://connectrpc.com/docs/protocol
type Code = connectv2.Code

// The zero code in gRPC is OK, which indicates that the operation was a
// success. We don't define a constant for it because it overlaps awkwardly
// with Go's error semantics: what does it mean to have a non-nil error with
// an OK status? (Also, the Connect protocol doesn't use a code for
// successes.)
const (
	// CodeCanceled indicates that the operation was canceled, typically by the
	// caller.
	CodeCanceled = connectv2.CodeCanceled

	// CodeUnknown indicates that the operation failed for an unknown reason.
	CodeUnknown = connectv2.CodeUnknown

	// CodeInvalidArgument indicates that client supplied an invalid argument.
	CodeInvalidArgument = connectv2.CodeInvalidArgument

	// CodeDeadlineExceeded indicates that deadline expired before the operation
	// could complete.
	CodeDeadlineExceeded = connectv2.CodeDeadlineExceeded

	// CodeNotFound indicates that some requested entity (for example, a file or
	// directory) was not found.
	CodeNotFound = connectv2.CodeNotFound

	// CodeAlreadyExists indicates that client attempted to create an entity (for
	// example, a file or directory) that already exists.
	CodeAlreadyExists = connectv2.CodeAlreadyExists

	// CodePermissionDenied indicates that the caller doesn't have permission to
	// execute the specified operation.
	CodePermissionDenied = connectv2.CodePermissionDenied

	// CodeResourceExhausted indicates that some resource has been exhausted. For
	// example, a per-user quota may be exhausted or the entire file system may
	// be full.
	CodeResourceExhausted = connectv2.CodeResourceExhausted

	// CodeFailedPrecondition indicates that the system is not in a state
	// required for the operation's execution.
	CodeFailedPrecondition = connectv2.CodeFailedPrecondition

	// CodeAborted indicates that operation was aborted by the system, usually
	// because of a concurrency issue such as a sequencer check failure or
	// transaction abort.
	CodeAborted = connectv2.CodeAborted

	// CodeOutOfRange indicates that the operation was attempted past the valid
	// range (for example, seeking past end-of-file).
	CodeOutOfRange = connectv2.CodeOutOfRange

	// CodeUnimplemented indicates that the operation isn't implemented,
	// supported, or enabled in this service.
	CodeUnimplemented = connectv2.CodeUnimplemented

	// CodeInternal indicates that some invariants expected by the underlying
	// system have been broken. This code is reserved for serious errors.
	CodeInternal = connectv2.CodeInternal

	// CodeUnavailable indicates that the service is currently unavailable. This
	// is usually temporary, so clients can back off and retry idempotent
	// operations.
	CodeUnavailable = connectv2.CodeUnavailable

	// CodeDataLoss indicates that the operation has resulted in unrecoverable
	// data loss or corruption.
	CodeDataLoss = connectv2.CodeDataLoss

	// CodeUnauthenticated indicates that the request does not have valid
	// authentication credentials for the operation.
	CodeUnauthenticated = connectv2.CodeUnauthenticated

	minCode = CodeCanceled
	maxCode = CodeUnauthenticated
)

// CodeOf returns the error's status code if it is or wraps an [*Error] and
// [CodeUnknown] otherwise.
func CodeOf(err error) Code {
	if connectErr, ok := asError(err); ok {
		return connectErr.Code()
	}
	return CodeUnknown
}
