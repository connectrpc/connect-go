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
	"testing"

	"connectrpc.com/connect/internal/assert"
)

func TestParseProtobufURL(t *testing.T) {
	assertExtractedProtobufPath(
		t,
		// full URL
		"https://api.foo.com/grpc/foo.user.v1.UserService/GetUser",
		"/foo.user.v1.UserService/GetUser",
	)
	assertExtractedProtobufPath(
		t,
		// rooted path
		"/foo.user.v1.UserService/GetUser",
		"/foo.user.v1.UserService/GetUser",
	)
	assertExtractedProtobufPath(
		t,
		// path without leading or trailing slashes
		"foo.user.v1.UserService/GetUser",
		"/foo.user.v1.UserService/GetUser",
	)
	assertExtractedProtobufPath(
		t,
		// path with trailing slash
		"/foo.user.v1.UserService.GetUser/",
		"/foo.user.v1.UserService.GetUser",
	)
	// edge cases
	assertExtractedProtobufPath(t, "", "/")
	assertExtractedProtobufPath(t, "//", "/")
}

func assertExtractedProtobufPath(t testing.TB, inputURL, expectPath string) {
	t.Helper()
	assert.Equal(
		t,
		extractProtobufPath(inputURL),
		expectPath,
	)
}
