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
	"net/http"
)

// NewReflectionHandler uses the information in the supplied Registrar to
// construct an HTTP handler for gRPC's server reflection API. It returns the
// path on which to mount the handler and the handler itself.
//
// Note that because the reflection API requires bidirectional streaming, the
// returned handler only supports gRPC over HTTP/2 (i.e., it doesn't support
// gRPC-Web). Also keep in mind that the reflection service exposes every
// protobuf package compiled into your binary - think twice before exposing it
// outside your organization.
//
// For more information, see:
// https://github.com/grpc/grpc-go/blob/master/Documentation/server-reflection-tutorial.md,
// https://github.com/grpc/grpc/blob/master/doc/server-reflection.md, and
// https://github.com/fullstorydev/grpcurl.
func NewReflectionHandler(registrar *Registrar, options ...HandlerOption) (string, http.Handler) {
	const serviceName = "/grpc.reflection.v1alpha.ServerReflection/"
	return serviceName, NewBidiStreamHandler(
		serviceName+"ServerReflectionInfo",
		registrar.serverReflectionInfo,
		options...,
	)
}
