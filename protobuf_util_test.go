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
	"fmt"
	"testing"

	"github.com/bufbuild/connect/internal/assert"
)

func TestParseProtobufURL(t *testing.T) {
	assertParsedProtobufURL(
		t,
		"https://api.foo.com/grpc/foo.user.v1.UserService/GetUser",
		"foo.user.v1.UserService",
		"/foo.user.v1.UserService/GetUser",
	)
	assertParsedProtobufURL(
		t,
		"/foo.user.v1.UserService/GetUser",
		"foo.user.v1.UserService",
		"/foo.user.v1.UserService/GetUser",
	)
	assertParsedProtobufURL(
		t,
		"foo.user.v1.UserService/GetUser",
		"foo.user.v1.UserService",
		"/foo.user.v1.UserService/GetUser",
	)
	assertParsedProtobufURL(t, "", "", "/")
	assertParsedProtobufURL(t, "//", "", "/")
	assertParsedProtobufURL(
		t,
		"/foo.user.v1.UserService.GetUser/",
		"foo.user.v1.UserService.GetUser",
		"/foo.user.v1.UserService.GetUser",
	)
}

func assertParsedProtobufURL(t testing.TB, inputURL, expectService, expectPath string) {
	t.Helper()
	assert.Equal(
		t,
		parseProtobufURL(inputURL),
		&parsedProtobufURL{
			FullyQualifiedServiceName: expectService,
			ProtoPath:                 expectPath,
		},
		fmt.Sprintf("%q", inputURL),
	)
}
