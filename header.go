package rerpc

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/textproto"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	spaceByte   = ' '
	tildeByte   = '~'
	percentByte = '%'
)

// IsReservedHeader checks whether the supplied key is reserved for use by
// reRPC, gRPC, or Twirp. Keys are canonicalized using
// textproto.CanonicalMIMEHeaderKey before checking. Unreserved headers are
// available for use by applications, but exercise caution: setting widely-used
// HTTP headers (e.g., Transfer-Encoding, Content-Length) may break your
// application in unexpected and difficult-to-debug ways.
//
// The signature of IsReservedHeader obeys semantic versioning, but the list of
// reserved headers may expand in minor releases to keep up with evolutions of
// the gRPC and Twirp protocols. To minimize the chance of breakage,
// applications should namespace their headers with a consistent prefix (e.g.,
// "Google-Cloud-").
//
// Current, the following keys are reserved: Accept, Accept-Encoding,
// Accept-Post, Allow, Content-Encoding, Content-Type, and Te. Keys prefixed
// with "Grpc-", "Rerpc-", and "Twirp-" are also reserved.
func IsReservedHeader(key string) error {
	canonical := textproto.CanonicalMIMEHeaderKey(key)
	switch canonical {
	case "Accept", "Accept-Encoding", "Accept-Post",
		"Allow",
		"Content-Encoding", "Content-Type",
		"Te":
		return fmt.Errorf("%q is a reserved header", key)
	}
	switch {
	case strings.HasPrefix(canonical, "Grpc-"):
		return fmt.Errorf("%q is reserved for the gRPC protocol", key)
	case strings.HasPrefix(canonical, "Rerpc-"):
		return fmt.Errorf("%q is reserved for future use by reRPC", key)
	case strings.HasPrefix(canonical, "Twirp-"):
		return fmt.Errorf("%q is reserved for future use by the Twirp protocol", key)
	default:
		return nil
	}
}

func encodeBinaryHeader(data []byte) string {
	// Implementations should emit unpadded values.
	return base64.RawStdEncoding.EncodeToString(data)
}

func decodeBinaryHeader(data string) ([]byte, error) {
	if len(data)%4 != 0 {
		// Data definitely isn't padded.
		return base64.RawStdEncoding.DecodeString(data)
	}
	// Either the data was padded, or padding wasn't necessary. In both cases,
	// the padding-aware decoder works.
	return base64.StdEncoding.DecodeString(data)
}

// percentEncode follows RFC 3986 Section 2.1 and the gRPC HTTP/2 spec. It's a
// variant of URL-encoding with fewer reserved characters. It's intended to
// take UTF-8 encoded text and escape non-ASCII bytes so that they're valid
// HTTP/1 headers, while still maximizing readability of the data on the wire.
//
// The grpc-message trailer (used for human-readable error messages) should be
// percent-encoded.
//
// References:
//   https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#responses
//   https://datatracker.ietf.org/doc/html/rfc3986#section-2.1
func percentEncode(msg string) string {
	for i := 0; i < len(msg); i++ {
		// Characters that need to be escaped are defined in gRPC's HTTP/2 spec.
		// They're different from the generic set defined in RFC 3986.
		if c := msg[i]; c < ' ' || c > '~' || c == '%' {
			return percentEncodeSlow(msg, i)
		}
	}
	return msg
}

// msg needs some percent-escaping. Bytes before offset don't require
// percent-encoding, so they can be copied to the output as-is.
func percentEncodeSlow(msg string, offset int) string {
	// TODO: pool these buffers
	// worst-case, percent-encoding triples length
	out := bytes.NewBuffer(make([]byte, 0, len(msg)*3))
	out.WriteString(msg[:offset])
	for i := offset; i < len(msg); i++ {
		c := msg[i]
		if c < ' ' || c > '~' || c == '%' {
			out.WriteString(fmt.Sprintf("%%%02X", c))
			continue
		}
		out.WriteByte(c)
	}
	return out.String()
}

func percentDecode(encoded string) string {
	for i := 0; i < len(encoded); i++ {
		if c := encoded[i]; c == '%' && i+2 < len(encoded) {
			return percentDecodeSlow(encoded, i)
		}
	}
	return encoded
}

// Similar to percentEncodeSlow: encoded is percent-encoded, and needs to be
// decoded byte-by-byte starting at offset.
func percentDecodeSlow(encoded string, offset int) string {
	// TODO: pool these buffers
	out := bytes.NewBuffer(make([]byte, 0, len(encoded)))
	out.WriteString(encoded[:offset])
	for i := offset; i < len(encoded); i++ {
		c := encoded[i]
		if c != '%' || i+2 >= len(encoded) {
			out.WriteByte(c)
			continue
		}
		parsed, err := strconv.ParseUint(encoded[i+1:i+3], 16 /* hex */, 8 /* bitsize */)
		if err != nil {
			fmt.Println(err)
			out.WriteRune(utf8.RuneError)
		} else {
			out.WriteByte(byte(parsed))
		}
		i += 2
	}
	return out.String()
}
