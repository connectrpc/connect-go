// Copyright 2021-2022 Buf Technologies, Inc.
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
	"fmt"
)

const (
	// The zero code is OK, which indicates that the operation was a success. We
	// don't define a constant for it because it overlaps awkwardly with Go's
	// error semantics: what does it mean to have a non-nil error with an OK
	// status?

	// CodeCanceled indicates that the operation was canceled, typically by the
	// caller.
	//
	// Note that connect follows the gRPC specification (and some, but not all,
	// implementations) and uses the British "CANCELLED" as CodeCanceled's string
	// representation rather than the American "CANCELED".
	CodeCanceled Code = 1

	// CodeUnknown indicates that the operation failed for an unknown reason.
	CodeUnknown Code = 2

	// CodeInvalidArgument indicates that client supplied an invalid argument.
	//
	// Note that this differs from CodeFailedPrecondition: CodeInvalidArgument
	// indicates that the argument(s) are problematic regardless of the state of
	// the system (for example, an invalid URL).
	CodeInvalidArgument Code = 3

	// CodeDeadlineExceeded indicates that deadline expired before the operation
	// could complete. For operations that change the state of the system, this
	// error may be returned even if the operation has completed successfully
	// (but late).
	CodeDeadlineExceeded Code = 4

	// CodeNotFound indicates that some requested entity (for example, a file or
	// directory) was not found.
	//
	// If an operation is denied for an entire class of users, such as gradual
	// feature rollout or an undocumented allowlist, CodeNotFound may be used. If
	// a request is denied for some users within a class of users, such as
	// user-based access control, CodePermissionDenied must be used.
	CodeNotFound Code = 5

	// CodeAlreadyExists indicates that client attempted to create an entity (for
	// example, a file or directory) that already exists.
	CodeAlreadyExists Code = 6

	// CodePermissionDenied indicates that the caller does'nt have permission to
	// execute the specified operation.
	//
	// CodePermissionDenied must not be used for rejections caused by exhausting
	// some resource (use CodeResourceExhausted instead). CodePermissionDenied
	// must not be used if the caller can't be identified (use
	// CodeUnauthenticated instead). This error code doesn't imply that the
	// request is valid, the requested entity exists, or other preconditions are
	// satisfied.
	CodePermissionDenied Code = 7

	// CodeResourceExhausted indicates that some resource has been exhausted. For
	// example, a per-user quota may be exhausted or the entire file system may
	// be full.
	CodeResourceExhausted Code = 8

	// CodeFailedPrecondition indicates that the system is not in a state
	// required for the operation's execution.
	//
	// Service implementors can use the following guidelines to decide between
	// CodeFailedPrecondition, CodeAborted, and CodeUnavailable:
	//
	//   - Use CodeUnavailable if the client can retry just the failing call.
	//   - Use CodeAborted if the client should retry at a higher level. For
	//   example, if a client-specified test-and-set fails, the client should
	//   restart the whole read-modify-write sequence.
	//   - Use CodeFailedPrecondition if the client should not retry until the
	//   system state has been explicitly fixed. For example, a deleting a
	//   directory on the filesystem might return CodeFailedPrecondition if the
	//   directory still contains files, since the client should not retry unless
	//   they first delete the offending files.
	CodeFailedPrecondition Code = 9

	// CodeAborted indicates that operation was aborted by the system, usually
	// because of a concurrency issue such as a sequencer check failure or
	// transaction abort.
	//
	// The documentation for CodeFailedPrecondition includes guidelines for
	// choosing between CodeFailedPrecondition, CodeAborted, and CodeUnavailable.
	CodeAborted Code = 10

	// CodeOutOfRange indicates that the operation was attempted past the valid
	// range (for example, seeking past end-of-file).
	//
	// Unlike CodeInvalidArgument, this error indicates a problem that may be
	// fixed if the system state changes. For example, a 32-bit file system will
	// generate CodeInvalidArgument if asked to read at an offset that is not in
	// the range [0,2^32), but it will generate CodeOutOfRange if asked to read
	// from an offset past the current file size.
	//
	// CodeOutOfRange naturally overlaps with CodeFailedPrecondition. Where
	// possible, use the more specific CodeOutOfRange so that callers who are
	// iterating through a space can easily detect when they're done.
	CodeOutOfRange Code = 11

	// CodeUnimplemented indicates that the operation isn't implemented,
	// supported, or enabled in this service.
	CodeUnimplemented Code = 12

	// CodeInternal indicates that some invariants expected by the underlying
	// system have been broken. This code is reserved for serious errors.
	CodeInternal Code = 13

	// CodeUnavailable indicates that the service is currently unavailable. This
	// is usually temporary, so clients can back off and retry idempotent
	// operations.
	CodeUnavailable Code = 14

	// CodeDataLoss indicates that the operation has resulted in unrecoverable
	// data loss or corruption.
	CodeDataLoss Code = 15

	// CodeUnauthenticated indicates that the request does not have valid
	// authentication credentials for the operation.
	CodeUnauthenticated Code = 16

	minCode Code = CodeUnknown
	maxCode Code = CodeUnauthenticated
)

// A Code is one of gRPC's canonical status codes. There are no user-defined
// codes, so only the codes enumerated below are valid.
//
// See the specification at
// https://github.com/grpc/grpc/blob/master/doc/statuscodes.md for detailed
// descriptions of each code and example usage.
type Code uint32

func (c Code) String() string {
	switch c {
	case CodeCanceled:
		return "Canceled"
	case CodeUnknown:
		return "Unknown"
	case CodeInvalidArgument:
		return "InvalidArgument"
	case CodeDeadlineExceeded:
		return "DeadlineExceeded"
	case CodeNotFound:
		return "NotFound"
	case CodeAlreadyExists:
		return "AlreadyExists"
	case CodePermissionDenied:
		return "PermissionDenied"
	case CodeResourceExhausted:
		return "ResourceExhausted"
	case CodeFailedPrecondition:
		return "FailedPrecondition"
	case CodeAborted:
		return "Aborted"
	case CodeOutOfRange:
		return "OutOfRange"
	case CodeUnimplemented:
		return "Unimplemented"
	case CodeInternal:
		return "Internal"
	case CodeUnavailable:
		return "Unavailable"
	case CodeDataLoss:
		return "DataLoss"
	case CodeUnauthenticated:
		return "Unauthenticated"
	}
	return fmt.Sprintf("Code(%d)", c)
}

func httpToCode(httpCode int) Code {
	// https://github.com/grpc/grpc/blob/master/doc/http-grpc-status-mapping.md
	// Note that this is not just the inverse of the gRPC-to-HTTP mapping.
	switch httpCode {
	case 400:
		return CodeInternal
	case 401:
		return CodeUnauthenticated
	case 403:
		return CodePermissionDenied
	case 404:
		return CodeUnimplemented
	case 429:
		return CodeUnavailable
	case 502:
		return CodeUnavailable
	case 503:
		return CodeUnavailable
	case 504:
		return CodeUnavailable
	default:
		return CodeUnknown
	}
}
