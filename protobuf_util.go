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
	"strings"
)

type parsedProtobufURL struct {
	// FullyQualifiedServiceName is fully-qualified protobuf service name (for
	// example, "acme.user.v1.UserService"). Connect uses this for reflection.
	FullyQualifiedServiceName string
	// ProtoPath is the trailing portion of the URL's path that corresponds to
	// the protobuf package, service, and method. It always starts with a slash.
	// Within connect, we use this as (1) Specification.Procedure and (2) the
	// path when mounting handlers on muxes.
	ProtoPath string
}

func parseProtobufURL(url string) *parsedProtobufURL {
	segments := strings.Split(url, "/")
	var pkg, method string
	if len(segments) > 0 {
		pkg = segments[0]
	}
	if len(segments) > 1 {
		pkg = segments[len(segments)-2]
		method = segments[len(segments)-1]
	}
	if pkg == "" {
		return &parsedProtobufURL{
			FullyQualifiedServiceName: "",
			ProtoPath:                 "/",
		}
	}
	if method == "" {
		return &parsedProtobufURL{
			FullyQualifiedServiceName: pkg,
			ProtoPath:                 "/" + pkg,
		}
	}
	return &parsedProtobufURL{
		FullyQualifiedServiceName: pkg,
		ProtoPath:                 "/" + pkg + "/" + method,
	}
}
