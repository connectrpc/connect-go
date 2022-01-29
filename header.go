package rerpc

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"
)

// IsValidHeaderKey checks whether the supplied key uses a safe subset of
// ASCII. (The HTTP specification doesn't define the encoding of headers, so
// non-ASCII values may be interpreted unexpectedly by other servers, clients,
// or proxies.) This function is purely advisory.
//
// reRPC uses the same safe ASCII subset for keys as gRPC: a-z, A-Z, 0-9, "-"
// (hyphen-minus), "_" (underscore), and "." (period).
//
// Keep in mind that setting headers meaningful to the underlying protocols may
// break your application in unexpected or difficult-to-debug ways. For
// example, you probably shouldn't set Transfer-Encoding or
// Grpc-Accept-Encoding unless you definitely want the accompanying semantics.
//
// If you'd like to be conservative, namespace your headers with an
// organization-specific prefix (e.g., "BufBuild-").
func IsValidHeaderKey(key string) error {
	if key == "" {
		return errors.New("empty header key")
	}
	if key[0] == ':' {
		return fmt.Errorf("%q is a reserved HTTP2 pseudo-header", key)
	}
	for _, c := range key {
		if !(('a' <= c && c <= 'z') ||
			('A' <= c && c <= 'Z') ||
			('0' <= c && c <= '9') ||
			c == '-' || c == '_' || c == '.') {
			return fmt.Errorf("%q contains non-ASCII or reserved characters", key)
		}
	}
	return nil
}

// IsValidHeaderValue checks whether the supplied string uses a safe subset of
// ASCII. (The HTTP specification doesn't define the encoding of headers, so
// non-ASCII values may be interpreted unexpectedly by other servers, clients,
// or proxies.) This function is purely advisory.
//
// reRPC uses the same safe ASCII subset for values as gRPC: only space and
// printable ASCII is allowed. Use EncodeBinaryHeader and DecodeBinaryHeader to
// send raw bytes.
func IsValidHeaderValue(v string) error {
	for i := range v {
		c := v[i]
		if c < 0x20 || c > 0x7E { // hex makes matching the gRPC spec easier
			return fmt.Errorf("%q isn't a valid header value: index %d is neither space nor printable ASCII", v, i)
		}
	}
	return nil
}

// EncodeBinaryHeader base64-encodes the data. It always emits unpadded values.
//
// For interoperability with Google's gRPC implementations, binary headers
// should have keys ending in "-Bin".
func EncodeBinaryHeader(data []byte) string {
	// gRPC specification says that implementations should emit unpadded values.
	return base64.RawStdEncoding.EncodeToString(data)
}

// DecodeBinaryHeader base64-decodes the data. It can decode padded or unpadded
// values.
//
// Binary headers sent by Google's gRPC implementations always have keys ending
// in "-Bin".
func DecodeBinaryHeader(data string) ([]byte, error) {
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
	out := getBuffer()
	defer putBuffer(out)
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
	out := getBuffer()
	defer putBuffer(out)
	out.WriteString(encoded[:offset])
	for i := offset; i < len(encoded); i++ {
		c := encoded[i]
		if c != '%' || i+2 >= len(encoded) {
			out.WriteByte(c)
			continue
		}
		parsed, err := strconv.ParseUint(encoded[i+1:i+3], 16 /* hex */, 8 /* bitsize */)
		if err != nil {
			out.WriteRune(utf8.RuneError)
		} else {
			out.WriteByte(byte(parsed))
		}
		i += 2
	}
	return out.String()
}

func mergeHeaders(into, from http.Header) {
	for k, vals := range from {
		into[k] = append(into[k], vals...)
	}
}
