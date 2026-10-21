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
	"fmt"
	"net/http"
	"runtime"

	connectv2 "connectrpc.com/connect/v2"
)

const (
	headerUserAgent  = "User-Agent"
	headerXUserAgent = "X-User-Agent"
	grpcHeaderStatus = "Grpc-Status"
)

var (
	// defaultConnectUserAgent returns a User-Agent string similar to those used in gRPC.
	//
	//nolint:gochecknoglobals
	defaultConnectUserAgent = fmt.Sprintf("connect-go/%s (%s)", Version, runtime.Version())
	// defaultGrpcUserAgent follows
	// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#user-agents.
	//
	//nolint:gochecknoglobals
	defaultGrpcUserAgent = fmt.Sprintf("grpc-go-connect/%s (%s)", Version, runtime.Version())
)

// EncodeBinaryHeader base64-encodes the data. It always emits unpadded values.
//
// In the Connect, gRPC, and gRPC-Web protocols, binary headers must have keys
// ending in "-Bin".
func EncodeBinaryHeader(data []byte) string {
	return connectv2.EncodeBinaryHeader(data)
}

// DecodeBinaryHeader base64-decodes the data. It can decode padded or
// unpadded values. Following usual HTTP semantics, multiple base64-encoded
// values may be joined with a comma. When receiving such comma-separated
// values, split them with [strings.Split] before calling DecodeBinaryHeader.
//
// Binary headers sent using the Connect, gRPC, and gRPC-Web protocols have
// keys ending in "-Bin".
func DecodeBinaryHeader(data string) ([]byte, error) {
	return connectv2.DecodeBinaryHeader(data)
}

// mergeHeaders copies entries from src into dst, appending values. Empty
// value slices are skipped to match net/http's pre-population of trailer
// headers.
func mergeHeaders(dst, src http.Header) {
	for k, vs := range src {
		if len(vs) == 0 {
			continue
		}
		dst[k] = append(dst[k], vs...)
	}
}
