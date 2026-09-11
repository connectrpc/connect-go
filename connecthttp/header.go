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

package connecthttp

import (
	"net/http"

	"connectrpc.com/connect/v2"
)

//nolint:gochecknoglobals
var protocolHeaders = map[string]struct{}{
	// HTTP headers.
	headerContentType:     {},
	headerContentLength:   {},
	headerContentEncoding: {},
	headerTrailer:         {},
	headerDate:            {},
	// Connect headers.
	connectUnaryHeaderAcceptCompression:     {},
	connectUnaryTrailerPrefix:               {},
	connectStreamingHeaderCompression:       {},
	connectStreamingHeaderAcceptCompression: {},
	connectHeaderTimeout:                    {},
	connectHeaderProtocolVersion:            {},
	// gRPC headers.
	grpcHeaderCompression:       {},
	grpcHeaderAcceptCompression: {},
	grpcHeaderTimeout:           {},
	grpcHeaderStatus:            {},
	grpcHeaderMessage:           {},
	grpcHeaderDetails:           {},
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

// getHeaderCanonical is a shortcut for Header.Get() which
// bypasses the CanonicalMIMEHeaderKey operation when we
// know the key is already in canonical form.
func getHeaderCanonical(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	v := h[key]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// setUserAgentIfAbsent sets the User-Agent header to the given value, but
// only if the caller has not already provided one. We check for the key's
// presence rather than comparing getHeaderCanonical(...) to "" so that a
// caller can suppress the default entirely by explicitly setting a nil/empty
// slice of values or an empty value.
func setUserAgentIfAbsent(header http.Header, value string) {
	// headerUserAgent is already in canonical form, so we can index the map
	// directly and bypass the checks in Header.Get and Header.Set.
	if _, ok := header[headerUserAgent]; !ok {
		header[headerUserAgent] = []string{value}
	}
}

// setHeaderCanonical is a shortcut for Header.Set() which
// bypasses the CanonicalMIMEHeaderKey operation when we
// know the key is already in canonical form.
func setHeaderCanonical(h http.Header, key, value string) {
	h[key] = []string{value}
}

// delHeaderCanonical is a shortcut for Header.Del() which
// bypasses the CanonicalMIMEHeaderKey operation when we
// know the key is already in canonical form.
func delHeaderCanonical(h http.Header, key string) {
	delete(h, key)
}

// toHTTPHeader merges header into the HTTP header h, excluding protocol
// headers so users can't override them.
func toHTTPHeader(h http.Header, header *connect.Header) {
	for key, vals := range header.All() {
		if _, isProtocolHeader := protocolHeaders[key]; isProtocolHeader {
			continue
		}
		h[key] = append(h[key], vals...)
	}
}

// fromHTTPHeader copies the HTTP header h into header.
func fromHTTPHeader(header *connect.Header, h http.Header) {
	for key, vals := range h {
		header.SetValues(key, vals)
	}
}
