package reflection_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"github.com/rerpc/rerpc"
	"github.com/rerpc/rerpc/health"
	"github.com/rerpc/rerpc/internal/assert"
	pingrpc "github.com/rerpc/rerpc/internal/gen/proto/go-rerpc/rerpc/ping/v1test"
	reflectionpb "github.com/rerpc/rerpc/internal/gen/proto/go/grpc/reflection/v1alpha"
	pingpb "github.com/rerpc/rerpc/internal/gen/proto/go/rerpc/ping/v1test"
	"github.com/rerpc/rerpc/reflection"
)

type pingServer struct {
	pingrpc.UnimplementedPingServiceServer
}

func TestReflection(t *testing.T) {
	reg := rerpc.NewRegistrar()
	mux := rerpc.NewServeMux(
		pingrpc.NewFullPingServiceHandler(pingServer{}, reg),
		health.NewHandler(health.NewChecker(reg)),
		reflection.NewHandler(reg),
	)

	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	pingRequestFQN := string((&pingpb.PingRequest{}).ProtoReflect().Descriptor().FullName())
	assert.Equal(t, reg.Services(), []string{
		"rerpc.ping.v1test.PingService",
	}, "services registered in memory")

	// TODO: Build this simplification into base package.
	detailed := rerpc.NewClientFunc[
		reflectionpb.ServerReflectionRequest,
		reflectionpb.ServerReflectionResponse,
	](
		server.Client(),
		server.URL,
		"grpc.reflection.v1alpha", "ServerReflection", "ServerReflectionInfo",
	)
	call := func(req *reflectionpb.ServerReflectionRequest) (*reflectionpb.ServerReflectionResponse, error) {
		res, err := detailed(context.Background(), rerpc.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, err
	}

	t.Run("list_services", func(t *testing.T) {
		req := &reflectionpb.ServerReflectionRequest{
			Host: "some-host",
			MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{
				ListServices: "ignored per proto documentation",
			},
		}
		res, err := call(req)
		assert.Nil(t, err, "reflection RPC error")
		expect := &reflectionpb.ServerReflectionResponse{
			ValidHost:       req.Host,
			OriginalRequest: req,
			MessageResponse: &reflectionpb.ServerReflectionResponse_ListServicesResponse{
				ListServicesResponse: &reflectionpb.ListServiceResponse{
					Service: []*reflectionpb.ServiceResponse{
						{Name: "rerpc.ping.v1test.PingService"},
					},
				},
			},
		}
		assert.Equal(t, res, expect, "response")
	})
	t.Run("file_by_filename", func(t *testing.T) {
		req := &reflectionpb.ServerReflectionRequest{
			Host: "some-host",
			MessageRequest: &reflectionpb.ServerReflectionRequest_FileByFilename{
				FileByFilename: "rerpc/ping/v1test/ping.proto",
			},
		}
		res, err := call(req)
		assert.Nil(t, err, "reflection RPC error")
		assert.Nil(t, res.GetErrorResponse(), "error in response")
		fds := res.GetFileDescriptorResponse()
		assert.NotNil(t, fds, "file descriptor response")
		assert.Equal(t, len(fds.FileDescriptorProto), 1, "number of fds returned")
		assert.True(
			t,
			bytes.Contains(fds.FileDescriptorProto[0], []byte(pingRequestFQN)),
			"fd should contain PingRequest struct",
		)
	})
	t.Run("file_containing_symbol", func(t *testing.T) {
		req := &reflectionpb.ServerReflectionRequest{
			Host: "some-host",
			MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingSymbol{
				FileContainingSymbol: pingRequestFQN,
			},
		}
		res, err := call(req)
		assert.Nil(t, err, "reflection RPC error")
		assert.Nil(t, res.GetErrorResponse(), "error in response")
		fds := res.GetFileDescriptorResponse()
		assert.NotNil(t, fds, "file descriptor response")
		assert.Equal(t, len(fds.FileDescriptorProto), 1, "number of fds returned")
		assert.True(
			t,
			bytes.Contains(fds.FileDescriptorProto[0], []byte(pingRequestFQN)),
			"fd should contain PingRequest struct",
		)
	})
	t.Run("file_containing_extension", func(t *testing.T) {
		req := &reflectionpb.ServerReflectionRequest{
			Host: "some-host",
			MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingExtension{
				FileContainingExtension: &reflectionpb.ExtensionRequest{
					ContainingType:  pingRequestFQN,
					ExtensionNumber: 42,
				},
			},
		}
		res, err := call(req)
		assert.Nil(t, err, "reflection RPC error")
		msgerr := res.GetErrorResponse()
		assert.NotNil(t, msgerr, "error in response proto")
		assert.Equal(t, msgerr.ErrorCode, int32(rerpc.CodeNotFound), "error code")
		assert.NotZero(t, msgerr.ErrorMessage, "error message")
	})
	t.Run("all_extension_numbers_of_type", func(t *testing.T) {
		req := &reflectionpb.ServerReflectionRequest{
			Host: "some-host",
			MessageRequest: &reflectionpb.ServerReflectionRequest_AllExtensionNumbersOfType{
				AllExtensionNumbersOfType: pingRequestFQN,
			},
		}
		res, err := call(req)
		assert.Nil(t, err, "reflection RPC error")
		expect := &reflectionpb.ServerReflectionResponse{
			ValidHost:       req.Host,
			OriginalRequest: req,
			MessageResponse: &reflectionpb.ServerReflectionResponse_AllExtensionNumbersResponse{
				AllExtensionNumbersResponse: &reflectionpb.ExtensionNumberResponse{
					BaseTypeName: pingRequestFQN,
				},
			},
		}
		assert.Equal(t, res, expect, "response")
	})
}
