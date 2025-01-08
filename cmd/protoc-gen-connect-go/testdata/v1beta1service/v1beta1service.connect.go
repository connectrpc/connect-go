// Copyright 2021-2024 The Connect Authors
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

// This file tests varying casing of the service name and method name.

// Code generated by protoc-gen-connect-go. DO NOT EDIT.
//
// Source: v1beta1service.proto

package v1beta1service

import (
	connect "connectrpc.com/connect"
	context "context"
	errors "errors"
	http "net/http"
	strings "strings"
)

// This is a compile-time assertion to ensure that this generated file and the connect package are
// compatible. If you get a compiler error that this constant is not defined, this code was
// generated with a version of connect newer than the one compiled into your binary. You can fix the
// problem by either regenerating this code with an older version of connect or updating the connect
// version compiled into your binary.
const _ = connect.IsAtLeastVersion1_13_0

const (
	// ExampleV1betaName is the fully-qualified name of the ExampleV1beta service.
	ExampleV1betaName = "example.ExampleV1beta"
)

// These constants are the fully-qualified names of the RPCs defined in this package. They're
// exposed at runtime as Spec.Procedure and as the final two segments of the HTTP route.
//
// Note that these are different from the fully-qualified method names used by
// google.golang.org/protobuf/reflect/protoreflect. To convert from these constants to
// reflection-formatted method names, remove the leading slash and convert the remaining slash to a
// period.
const (
	// ExampleV1BetaMethodProcedure is the fully-qualified name of the ExampleV1beta's Method RPC.
	ExampleV1BetaMethodProcedure = "/example.ExampleV1beta/Method"
)

// ExampleV1BetaClient is a client for the example.ExampleV1beta service.
type ExampleV1BetaClient interface {
	Method(context.Context, *connect.Request[GetExample_Request]) (*connect.Response[Get1ExampleResponse], error)
}

// NewExampleV1BetaClient constructs a client for the example.ExampleV1beta service. By default, it
// uses the Connect protocol with the binary Protobuf Codec, asks for gzipped responses, and sends
// uncompressed requests. To use the gRPC or gRPC-Web protocols, supply the connect.WithGRPC() or
// connect.WithGRPCWeb() options.
//
// The URL supplied here should be the base URL for the Connect or gRPC server (for example,
// http://api.acme.com or https://acme.com/grpc).
func NewExampleV1BetaClient(httpClient connect.HTTPClient, baseURL string, opts ...connect.ClientOption) ExampleV1BetaClient {
	baseURL = strings.TrimRight(baseURL, "/")
	exampleV1BetaMethods := File_v1beta1service_proto.Services().ByName("ExampleV1beta").Methods()
	return &exampleV1BetaClient{
		method: connect.NewClient[GetExample_Request, Get1ExampleResponse](
			httpClient,
			baseURL+ExampleV1BetaMethodProcedure,
			connect.WithSchema(exampleV1BetaMethods.ByName("Method")),
			connect.WithClientOptions(opts...),
		),
	}
}

// exampleV1BetaClient implements ExampleV1BetaClient.
type exampleV1BetaClient struct {
	method *connect.Client[GetExample_Request, Get1ExampleResponse]
}

// Method calls example.ExampleV1beta.Method.
func (c *exampleV1BetaClient) Method(ctx context.Context, req *connect.Request[GetExample_Request]) (*connect.Response[Get1ExampleResponse], error) {
	return c.method.CallUnary(ctx, req)
}

// ExampleV1BetaHandler is an implementation of the example.ExampleV1beta service.
type ExampleV1BetaHandler interface {
	Method(context.Context, *connect.Request[GetExample_Request]) (*connect.Response[Get1ExampleResponse], error)
}

// NewExampleV1BetaHandler builds an HTTP handler from the service implementation. It returns the
// path on which to mount the handler and the handler itself.
//
// By default, handlers support the Connect, gRPC, and gRPC-Web protocols with the binary Protobuf
// and JSON codecs. They also support gzip compression.
func NewExampleV1BetaHandler(svc ExampleV1BetaHandler, opts ...connect.HandlerOption) (string, http.Handler) {
	exampleV1BetaMethods := File_v1beta1service_proto.Services().ByName("ExampleV1beta").Methods()
	exampleV1BetaMethodHandler := connect.NewUnaryHandler(
		ExampleV1BetaMethodProcedure,
		svc.Method,
		connect.WithSchema(exampleV1BetaMethods.ByName("Method")),
		connect.WithHandlerOptions(opts...),
	)
	return "/example.ExampleV1beta/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ExampleV1BetaMethodProcedure:
			exampleV1BetaMethodHandler.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// UnimplementedExampleV1BetaHandler returns CodeUnimplemented from all methods.
type UnimplementedExampleV1BetaHandler struct{}

func (UnimplementedExampleV1BetaHandler) Method(context.Context, *connect.Request[GetExample_Request]) (*connect.Response[Get1ExampleResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("example.ExampleV1beta.Method is not implemented"))
}
