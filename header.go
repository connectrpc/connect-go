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
	"net/http"

	connectv2 "connectrpc.com/connect/v2"
)

// protocolHeaders are the HTTP and protocol headers an error's metadata may
// not set.
//
//nolint:gochecknoglobals
var protocolHeaders = map[string]struct{}{
	"Content-Type":             {},
	"Content-Length":           {},
	"Content-Encoding":         {},
	"Host":                     {},
	"User-Agent":               {},
	"Trailer":                  {},
	"Date":                     {},
	"Accept-Encoding":          {},
	"Trailer-":                 {},
	"Connect-Content-Encoding": {},
	"Connect-Accept-Encoding":  {},
	"Connect-Timeout-Ms":       {},
	"Connect-Protocol-Version": {},
	"Grpc-Encoding":            {},
	"Grpc-Accept-Encoding":     {},
	"Grpc-Timeout":             {},
	"Grpc-Status":              {},
	"Grpc-Message":             {},
	"Grpc-Status-Details-Bin":  {},
}

// EncodeBinaryHeader base64-encodes the data. It always emits unpadded values.
//
// In the Connect, gRPC, and gRPC-Web protocols, binary headers must have keys
// ending in "-Bin".
func EncodeBinaryHeader(data []byte) string {
	return connectv2.EncodeBinaryHeader(data)
}

// DecodeBinaryHeader base64-decodes the data. It can decode padded or unpadded
// values. Following usual HTTP semantics, multiple base64-encoded values may
// be joined with a comma. When receiving such comma-separated values, split
// them with [strings.Split] before calling DecodeBinaryHeader.
//
// Binary headers sent using the Connect, gRPC, and gRPC-Web protocols have
// keys ending in "-Bin".
func DecodeBinaryHeader(data string) ([]byte, error) {
	return connectv2.DecodeBinaryHeader(data)
}

func mergeHeaders(into, from http.Header) {
	for key, vals := range from {
		if len(vals) == 0 {
			// For response trailers, net/http will pre-populate entries
			// with nil values based on the "Trailer" header. But if there
			// are no actual values for those keys, we skip them.
			continue
		}
		into[key] = append(into[key], vals...)
	}
}

// mergeNonProtocolHeaders merges headers excluding protocol headers defined in
// protocolHeaders.
func mergeNonProtocolHeaders(into, from http.Header) {
	for key, vals := range from {
		if len(vals) == 0 {
			// For response trailers, net/http will pre-populate entries
			// with nil values based on the "Trailer" header. But if there
			// are no actual values for those keys, we skip them.
			continue
		}
		if _, isProtocolHeader := protocolHeaders[key]; !isProtocolHeader {
			into[key] = append(into[key], vals...)
		}
	}
}
