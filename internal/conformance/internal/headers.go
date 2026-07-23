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

package internal

import (
	"connectrpc.com/connect/v2"
	conformancev1 "connectrpc.com/connect/v2/internal/conformance/internal/gen/connectrpc/conformance/v1"
)

// AddHeaders adds all header values in src to dest.
func AddHeaders(
	src []*conformancev1.Header,
	dest *connect.Header,
) {
	for _, header := range src {
		for _, val := range header.Value {
			dest.Add(header.Name, val)
		}
	}
}

// AddTrailers adds all header values in src to dest. In v2 trailers are carried
// in their own metadata, so this is equivalent to [AddHeaders].
func AddTrailers(
	src []*conformancev1.Header,
	dest *connect.Header,
) {
	AddHeaders(src, dest)
}

// ConvertToProtoHeader converts metadata to a slice of proto Headers.
func ConvertToProtoHeader(src *connect.Header) []*conformancev1.Header {
	if src == nil {
		return nil
	}
	headerInfo := make([]*conformancev1.Header, 0)
	for key, value := range src.All() {
		headerInfo = append(headerInfo, &conformancev1.Header{
			Name:  key,
			Value: value,
		})
	}
	return headerInfo
}
