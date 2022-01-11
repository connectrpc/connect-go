package rerpc

import (
	"fmt"
	"net/http"
	"strconv"
)

var (
	strToCode = map[string]Code{
		"OK":                  CodeOK,
		"CANCELLED":           CodeCanceled, // specification uses British spelling
		"UNKNOWN":             CodeUnknown,
		"INVALID_ARGUMENT":    CodeInvalidArgument,
		"DEADLINE_EXCEEDED":   CodeDeadlineExceeded,
		"NOT_FOUND":           CodeNotFound,
		"ALREADY_EXISTS":      CodeAlreadyExists,
		"PERMISSION_DENIED":   CodePermissionDenied,
		"RESOURCE_EXHAUSTED":  CodeResourceExhausted,
		"FAILED_PRECONDITION": CodeFailedPrecondition,
		"ABORTED":             CodeAborted,
		"OUT_OF_RANGE":        CodeOutOfRange,
		"UNIMPLEMENTED":       CodeUnimplemented,
		"INTERNAL":            CodeInternal,
		"UNAVAILABLE":         CodeUnavailable,
		"DATA_LOSS":           CodeDataLoss,
		"UNAUTHENTICATED":     CodeUnauthenticated,
	}
	grpcToHTTP = map[Code]int{
		// Codes are numbers rather than net/http constants to make copying from
		// the specification easy.
		CodeOK:                 200,
		CodeCanceled:           499,
		CodeUnknown:            500,
		CodeInvalidArgument:    400,
		CodeDeadlineExceeded:   504,
		CodeNotFound:           404,
		CodeAlreadyExists:      409,
		CodePermissionDenied:   403,
		CodeResourceExhausted:  429,
		CodeFailedPrecondition: 400,
		CodeAborted:            409,
		CodeOutOfRange:         400,
		CodeUnimplemented:      501,
		CodeInternal:           500,
		CodeUnavailable:        503,
		CodeDataLoss:           500,
		CodeUnauthenticated:    401,
	}
	grpcToTwirp = map[Code]string{
		CodeOK:                 "ok",
		CodeCanceled:           "canceled",
		CodeUnknown:            "unknown",
		CodeInvalidArgument:    "invalid_argument",
		CodeDeadlineExceeded:   "deadline_exceeded",
		CodeNotFound:           "not_found",
		CodeAlreadyExists:      "already_exists",
		CodePermissionDenied:   "permission_denied",
		CodeResourceExhausted:  "resource_exhausted",
		CodeFailedPrecondition: "failed_precondition",
		CodeAborted:            "aborted",
		CodeOutOfRange:         "out_of_range",
		CodeUnimplemented:      "unimplemented",
		CodeInternal:           "internal",
		CodeUnavailable:        "unavailable",
		CodeDataLoss:           "dataloss",
		CodeUnauthenticated:    "unauthenticated",
	}
	// From https://github.com/grpc/grpc/blob/master/doc/http-grpc-status-mapping.md.
	// Note that these are not the inverse of the previous mapping.
	httpToGRPC = map[int]Code{
		400: CodeInternal,
		401: CodeUnauthenticated,
		403: CodePermissionDenied,
		404: CodeUnimplemented,
		429: CodeUnavailable,
		502: CodeUnavailable,
		503: CodeUnavailable,
		504: CodeUnavailable,
		// all other HTTP status codes map to CodeUnknown
	}
)

// A Code is one of gRPC's canonical status codes. There are no user-defined
// codes, so only the codes enumerated below are valid.
//
// See the specification at
// https://github.com/grpc/grpc/blob/master/doc/statuscodes.md for detailed
// descriptions of each code and example usage.
type Code uint32

const (
	CodeOK                 Code = 0  // success
	CodeCanceled           Code = 1  // canceled, usually by the user
	CodeUnknown            Code = 2  // unknown error
	CodeInvalidArgument    Code = 3  // argument invalid regardless of system state
	CodeDeadlineExceeded   Code = 4  // operation expired, may or may not have completed
	CodeNotFound           Code = 5  // entity not found
	CodeAlreadyExists      Code = 6  // entity already exists
	CodePermissionDenied   Code = 7  // operation not authorized
	CodeResourceExhausted  Code = 8  // quota exhausted
	CodeFailedPrecondition Code = 9  // argument invalid in current system state
	CodeAborted            Code = 10 // operation aborted
	CodeOutOfRange         Code = 11 // out of bounds, use instead of CodeFailedPrecondition
	CodeUnimplemented      Code = 12 // operation not implemented or disabled
	CodeInternal           Code = 13 // internal error, reserved for "serious errors"
	CodeUnavailable        Code = 14 // unavailable, client should back off and retry
	CodeDataLoss           Code = 15 // unrecoverable data loss or corruption
	CodeUnauthenticated    Code = 16 // request isn't authenticated

	minCode Code = CodeOK
	maxCode Code = CodeUnauthenticated
)

// MarshalText implements encoding.TextMarshaler. Codes are marshaled in their
// numeric representations.
func (c Code) MarshalText() ([]byte, error) {
	if c < minCode || c > maxCode {
		return nil, fmt.Errorf("invalid code %v", c)
	}
	return []byte(strconv.Itoa(int(c))), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. It accepts both numeric
// representations (as produced by MarshalText) and the all-caps strings from
// the gRPC specification. Note that the specification uses the British
// "CANCELLED" for CodeCanceled.
func (c *Code) UnmarshalText(b []byte) error {
	if n, ok := strToCode[string(b)]; ok {
		*c = n
		return nil
	}
	n, err := strconv.ParseUint(string(b), 10 /* base */, 32 /* bitsize */)
	if err != nil {
		return fmt.Errorf("invalid code %q", string(b))
	}
	code := Code(n)
	if code < minCode || code > maxCode {
		return fmt.Errorf("invalid code %v", n)
	}
	*c = code
	return nil
}

func (c Code) http() int {
	if c < minCode || c > maxCode {
		// Code is invalid, which is definitely a 500.
		return http.StatusInternalServerError
	}
	return grpcToHTTP[c]
}

func (c Code) String() string {
	// golang.org/x/tools@v0.1.4 pulls in a random markdown renderer from
	// Github, so we're hand-writing this instead of using stringer.
	switch c {
	case CodeOK:
		return "OK"
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
