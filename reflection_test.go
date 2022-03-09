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

package connect_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bufbuild/connect"
	"github.com/bufbuild/connect/internal/assert"
	"github.com/bufbuild/connect/internal/gen/connect/connect/ping/v1/pingv1rpc"
	pingv1 "github.com/bufbuild/connect/internal/gen/go/connect/ping/v1"
	reflectionv1alpha1 "github.com/bufbuild/connect/internal/gen/go/connectext/grpc/reflection/v1alpha"
)

func TestReflection(t *testing.T) {
	reg := connect.NewRegistrar()
	mux := http.NewServeMux()
	mux.Handle(pingv1rpc.NewPingServiceHandler(
		pingv1rpc.UnimplementedPingServiceHandler{},
		connect.WithRegistrar(reg),
	))
	mux.Handle(connect.NewReflectionHandler(reg))

	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	pingRequestFQN := string((&pingv1.PingRequest{}).ProtoReflect().Descriptor().FullName())
	client, err := connect.NewClient[
		reflectionv1alpha1.ServerReflectionRequest,
		reflectionv1alpha1.ServerReflectionResponse,
	](
		server.Client(),
		server.URL+"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
		connect.WithGRPC(),
	)
	assert.Nil(t, err)
	call := func(req *reflectionv1alpha1.ServerReflectionRequest) (*reflectionv1alpha1.ServerReflectionResponse, error) {
		res, err := client.CallUnary(context.Background(), connect.NewEnvelope(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, err
	}

	t.Run("list_services", func(t *testing.T) {
		req := &reflectionv1alpha1.ServerReflectionRequest{
			Host: "some-host",
			MessageRequest: &reflectionv1alpha1.ServerReflectionRequest_ListServices{
				ListServices: "ignored per proto documentation",
			},
		}
		res, err := call(req)
		assert.Nil(t, err)
		expect := &reflectionv1alpha1.ServerReflectionResponse{
			ValidHost:       req.Host,
			OriginalRequest: req,
			MessageResponse: &reflectionv1alpha1.ServerReflectionResponse_ListServicesResponse{
				ListServicesResponse: &reflectionv1alpha1.ListServiceResponse{
					Service: []*reflectionv1alpha1.ServiceResponse{
						{Name: "connect.ping.v1.PingService"},
					},
				},
			},
		}
		assert.Equal(t, res, expect)
	})
	t.Run("file_by_filename", func(t *testing.T) {
		req := &reflectionv1alpha1.ServerReflectionRequest{
			Host: "some-host",
			MessageRequest: &reflectionv1alpha1.ServerReflectionRequest_FileByFilename{
				FileByFilename: "connect/ping/v1/ping.proto",
			},
		}
		res, err := call(req)
		assert.Nil(t, err)
		assert.Nil(t, res.GetErrorResponse())
		fds := res.GetFileDescriptorResponse()
		assert.NotNil(t, fds)
		assert.Equal(t, len(fds.FileDescriptorProto), 1)
		assert.True(
			t,
			bytes.Contains(fds.FileDescriptorProto[0], []byte(pingRequestFQN)),
		)
	})
	t.Run("file_containing_symbol", func(t *testing.T) {
		req := &reflectionv1alpha1.ServerReflectionRequest{
			Host: "some-host",
			MessageRequest: &reflectionv1alpha1.ServerReflectionRequest_FileContainingSymbol{
				FileContainingSymbol: pingRequestFQN,
			},
		}
		res, err := call(req)
		assert.Nil(t, err)
		assert.Nil(t, res.GetErrorResponse())
		fds := res.GetFileDescriptorResponse()
		assert.NotNil(t, fds)
		assert.Equal(t, len(fds.FileDescriptorProto), 1)
		assert.True(
			t,
			bytes.Contains(fds.FileDescriptorProto[0], []byte(pingRequestFQN)),
		)
	})
	t.Run("file_containing_extension", func(t *testing.T) {
		req := &reflectionv1alpha1.ServerReflectionRequest{
			Host: "some-host",
			MessageRequest: &reflectionv1alpha1.ServerReflectionRequest_FileContainingExtension{
				FileContainingExtension: &reflectionv1alpha1.ExtensionRequest{
					ContainingType:  pingRequestFQN,
					ExtensionNumber: 42,
				},
			},
		}
		res, err := call(req)
		assert.Nil(t, err)
		msgerr := res.GetErrorResponse()
		assert.NotNil(t, msgerr)
		assert.Equal(t, msgerr.ErrorCode, int32(connect.CodeNotFound))
		assert.NotZero(t, msgerr.ErrorMessage)
	})
	t.Run("all_extension_numbers_of_type", func(t *testing.T) {
		req := &reflectionv1alpha1.ServerReflectionRequest{
			Host: "some-host",
			MessageRequest: &reflectionv1alpha1.ServerReflectionRequest_AllExtensionNumbersOfType{
				AllExtensionNumbersOfType: pingRequestFQN,
			},
		}
		res, err := call(req)
		assert.Nil(t, err)
		expect := &reflectionv1alpha1.ServerReflectionResponse{
			ValidHost:       req.Host,
			OriginalRequest: req,
			MessageResponse: &reflectionv1alpha1.ServerReflectionResponse_AllExtensionNumbersResponse{
				AllExtensionNumbersResponse: &reflectionv1alpha1.ExtensionNumberResponse{
					BaseTypeName: pingRequestFQN,
				},
			},
		}
		assert.Equal(t, res, expect)
	})
}
