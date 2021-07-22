package rerpc

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strconv"
	"unicode/utf8"
)

const (
	spaceByte   = ' '
	tildeByte   = '~'
	percentByte = '%'
)

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

//func encodeGRPCMessage(msg string) string {
//	if msg == "" {
//		return ""
//	}
//	lenMsg := len(msg)
//	for i := 0; i < lenMsg; i++ {
//		c := msg[i]
//		if !(c >= spaceByte && c <= tildeByte && c != percentByte) {
//			return encodeGRPCMessageUnchecked(msg)
//		}
//	}
//	return msg
//}

//// See encodeGRPCMessage. This was also copied from grpc-go.
//func encodeGRPCMessageUnchecked(msg string) string {
//	var buf bytes.Buffer
//	for len(msg) > 0 {
//		r, size := utf8.DecodeRuneInString(msg)
//		rs := string(r)
//		for i := range rs {
//			b := rs[i]
//			if size > 1 {
//				// If size > 1, r is not ascii. Always do percent encoding.
//				buf.WriteString(fmt.Sprintf("%%%02X", b))
//				continue
//			}

//			// The for loop is necessary even if size == 1. r could be
//			// utf8.RuneError.
//			//
//			// fmt.Sprintf("%%%02X", utf8.RuneError) gives "%FFFD".
//			if b >= spaceByte && b <= tildeByte && b != percentByte {
//				buf.WriteByte(b)
//			} else {
//				buf.WriteString(fmt.Sprintf("%%%02X", b))
//			}
//		}
//		msg = msg[size:]
//	}
//	return buf.String()
//}

//// See encodeGRPCMessage. This was also copied from grpc-go.
//func decodeGRPCMessage(msg string) string {
//	if msg == "" {
//		return ""
//	}
//	lenMsg := len(msg)
//	for i := 0; i < lenMsg; i++ {
//		if msg[i] == percentByte && i+2 < lenMsg {
//			return decodeGRPCMessageUnchecked(msg)
//		}
//	}
//	return msg
//}

//// See encodeGRPCMessage. This was also copied from grpc-go.
//func decodeGRPCMessageUnchecked(msg string) string {
//	var buf bytes.Buffer
//	lenMsg := len(msg)
//	for i := 0; i < lenMsg; i++ {
//		c := msg[i]
//		if c == percentByte && i+2 < lenMsg {
//			parsed, err := strconv.ParseUint(msg[i+1:i+3], 16, 8)
//			if err != nil {
//				buf.WriteByte(c)
//			} else {
//				buf.WriteByte(byte(parsed))
//				i += 2
//			}
//		} else {
//			buf.WriteByte(c)
//		}
//	}
//	return buf.String()
//}
