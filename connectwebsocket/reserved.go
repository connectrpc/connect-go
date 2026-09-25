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

package connectwebsocket

import "strings"

// Reserved request-header names: the keys a client may not set from a
// Leading-Metadata message. A Leading-Metadata key otherwise replaces the
// upgrade request's value for that key, so this list is what keeps a peer from
// rewriting what the connection itself established.

// isFetchForbidden reports whether a lower-cased name is a forbidden
// request-header under the Fetch standard.
//
// The three method-override names are conditional there — forbidden only when
// the value names a forbidden method — and unconditional here, because the
// method they would override has no meaning on this transport.
func isFetchForbidden(lower string) bool {
	switch lower {
	case "accept-charset",
		"accept-encoding",
		"access-control-request-headers",
		"access-control-request-method",
		"connection",
		"content-length",
		"cookie",
		"cookie2",
		"date",
		"dnt",
		"expect",
		"host",
		"keep-alive",
		"origin",
		"referer",
		"set-cookie",
		"te",
		"trailer",
		"transfer-encoding",
		"upgrade",
		"via",
		"x-http-method",
		"x-http-method-override",
		"x-method-override":
		return true
	}
	return strings.HasPrefix(lower, "proxy-") || strings.HasPrefix(lower, "sec-")
}

// isProtocolControlled reports whether this binding owns the name itself. The
// sec- prefix above already covers the WebSocket handshake's own headers.
func isProtocolControlled(lower string) bool {
	return lower == "connect-protocol-version"
}

// defaultInfrastructureHeaders is what a proxy in front of the server sets and
// a client must not be able to forge. A deployment replaces this list with
// WithInfrastructureHeaders, because which names its own infrastructure
// controls is a property of that deployment.
func defaultInfrastructureHeaders() []string {
	return []string{"Forwarded", "X-Forwarded-*", "X-Real-IP"}
}

// reservedHeaderReason names why a key may not appear in a Leading-Metadata
// message, or returns false if it may.
func reservedHeaderReason(key string, infrastructure []string) (string, bool) {
	lower := strings.ToLower(key)
	if isFetchForbidden(lower) {
		return "forbidden as a request header by the Fetch standard", true
	}
	if isProtocolControlled(lower) {
		return "controlled by this protocol", true
	}
	for _, pattern := range infrastructure {
		if matchHeaderPattern(lower, strings.ToLower(pattern)) {
			return "on this server's infrastructure deny list", true
		}
	}
	return "", false
}

// matchHeaderPattern matches a lower-cased name against a lower-cased pattern,
// where a trailing * matches any suffix. Both are already lower-cased.
func matchHeaderPattern(name, pattern string) bool {
	if prefix, found := strings.CutSuffix(pattern, "*"); found {
		return strings.HasPrefix(name, prefix)
	}
	return name == pattern
}
