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
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"
)

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
func percentEncode(bufferPool *bufferPool, msg string) string {
	for i := 0; i < len(msg); i++ {
		// Characters that need to be escaped are defined in gRPC's HTTP/2 spec.
		// They're different from the generic set defined in RFC 3986.
		if c := msg[i]; c < ' ' || c > '~' || c == '%' {
			return percentEncodeSlow(bufferPool, msg, i)
		}
	}
	return msg
}

// msg needs some percent-escaping. Bytes before offset don't require
// percent-encoding, so they can be copied to the output as-is.
func percentEncodeSlow(bufferPool *bufferPool, msg string, offset int) string {
	out := bufferPool.Get()
	defer bufferPool.Put(out)
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

func percentDecode(bufferPool *bufferPool, encoded string) string {
	for i := 0; i < len(encoded); i++ {
		if c := encoded[i]; c == '%' && i+2 < len(encoded) {
			return percentDecodeSlow(bufferPool, encoded, i)
		}
	}
	return encoded
}

// Similar to percentEncodeSlow: encoded is percent-encoded, and needs to be
// decoded byte-by-byte starting at offset.
func percentDecodeSlow(bufferPool *bufferPool, encoded string, offset int) string {
	out := bufferPool.Get()
	defer bufferPool.Put(out)
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
