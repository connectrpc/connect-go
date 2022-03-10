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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"

	reflectionv1alpha1 "github.com/bufbuild/connect/internal/gen/go/connectext/grpc/reflection/v1alpha"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// A Registrar collects information to support gRPC server reflection
// when building handlers.
//
// Handlers will register themselves with a Registrar when the
// connect.WithRegistrar option is used.
//
//   registrar := connect.NewRegistrar()
//   mux := http.NewServeMux()
//   mux.Handle(
//     pingv1connect.NewPingServiceHandler(
//       &ExamplePingServer{},
//       connect.WithRegistrar(registrar),
//     ),
//   )
//   mux.Handle(registrar.NewReflectionHandler())
//   mux.Handle(registrar.NewHealthHandler())
type Registrar struct {
	mu       sync.RWMutex
	services map[string]struct{}
}

// NewRegistrar constructs an empty Registrar.
func NewRegistrar() *Registrar {
	return &Registrar{services: make(map[string]struct{})}
}

// NewReflectionHandler uses the information in the Registrar to
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
// https://github.com/grpc/grpc-go/blob/master/Documentation/server-reflection-tutorial.md
// https://github.com/grpc/grpc/blob/master/doc/server-reflection.md
// https://github.com/fullstorydev/grpcurl
func (r *Registrar) NewReflectionHandler() (string, http.Handler) {
	const serviceName = "/grpc.reflection.v1alpha.ServerReflection/"
	return serviceName, NewBidiStreamHandler(
		serviceName+"ServerReflectionInfo",
		r.serverReflectionInfo,
		&disableRegistrationOption{},
	)
}

// NewHealthHandler uses the information in the Registrar to construct  an HTTP
// handler for gRPC's health-checking API. It returns the path on which to mount the
// handler and the HTTP handler itself. It always returns HealthStatusServing for
// the process and all registered services.
//
// Note that the returned handler only supports the unary Check method, not the
// streaming Watch. As suggested in gRPC's health schema, connect returns
// CodeUnimplemented for the Watch method.
//
// For more details on gRPC's health checking protocol, see
// https://github.com/grpc/grpc/blob/master/doc/health-checking.md and
// https://github.com/grpc/grpc/blob/master/src/proto/grpc/health/v1/health.proto.
func (r *Registrar) NewHealthHandler() (string, http.Handler) {
	return NewHealthHandler(
		func(_ context.Context, service string) (HealthStatus, error) {
			if service == "" {
				return HealthStatusServing, nil
			}
			if r.isRegistered(service) {
				return HealthStatusServing, nil
			}
			return HealthStatusUnspecified, NewError(
				CodeNotFound,
				fmt.Errorf("unknown service %s", service),
			)
		},
	)
}

// serviceNames returns the fully-qualified names of the registered protobuf
// services. The returned slice is a copy, so it's safe for callers to modify.
// This method is safe to call concurrently.
func (r *Registrar) serviceNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.services))
	for name := range r.services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// isRegistered checks whether a fully-qualified protobuf service name is
// registered. It's safe to call concurrently.
func (r *Registrar) isRegistered(service string) bool {
	r.mu.RLock()
	_, ok := r.services[service]
	r.mu.RUnlock()
	return ok
}

// Registers a protobuf package and service combination. Safe to call
// concurrently.
func (r *Registrar) register(name string) {
	r.mu.Lock()
	r.services[name] = struct{}{}
	r.mu.Unlock()
}

// serverReflectionInfo implements the gRPC server reflection API.
func (r *Registrar) serverReflectionInfo(
	ctx context.Context,
	stream *BidiStream[
		reflectionv1alpha1.ServerReflectionRequest,
		reflectionv1alpha1.ServerReflectionResponse,
	],
) error {
	fileDescriptorsSent := &fileDescriptorNameSet{}
	for {
		request, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		// The grpc-go implementation of server reflection uses the APIs from
		// github.com/google/protobuf, which makes the logic fairly complex. The new
		// google.golang.org/protobuf/reflect/protoregistry exposes a higher-level
		// API that we'll use here.
		//
		// Note that the server reflection API sends file descriptors as uncompressed
		// proto-serialized bytes.
		response := &reflectionv1alpha1.ServerReflectionResponse{
			ValidHost:       request.Host,
			OriginalRequest: request,
		}
		switch messageRequest := request.MessageRequest.(type) {
		case *reflectionv1alpha1.ServerReflectionRequest_FileByFilename:
			data, err := getFileByFilename(messageRequest.FileByFilename, fileDescriptorsSent)
			if err != nil {
				response.MessageResponse = newNotFoundResponse(err)
			} else {
				response.MessageResponse = &reflectionv1alpha1.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &reflectionv1alpha1.FileDescriptorResponse{FileDescriptorProto: data},
				}
			}
		case *reflectionv1alpha1.ServerReflectionRequest_FileContainingSymbol:
			data, err := getFileContainingSymbol(
				messageRequest.FileContainingSymbol,
				fileDescriptorsSent,
			)
			if err != nil {
				response.MessageResponse = newNotFoundResponse(err)
			} else {
				response.MessageResponse = &reflectionv1alpha1.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &reflectionv1alpha1.FileDescriptorResponse{FileDescriptorProto: data},
				}
			}
		case *reflectionv1alpha1.ServerReflectionRequest_FileContainingExtension:
			msgFQN := messageRequest.FileContainingExtension.ContainingType
			extNumber := messageRequest.FileContainingExtension.ExtensionNumber
			data, err := getFileContainingExtension(msgFQN, extNumber, fileDescriptorsSent)
			if err != nil {
				response.MessageResponse = newNotFoundResponse(err)
			} else {
				response.MessageResponse = &reflectionv1alpha1.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &reflectionv1alpha1.FileDescriptorResponse{FileDescriptorProto: data},
				}
			}
		case *reflectionv1alpha1.ServerReflectionRequest_AllExtensionNumbersOfType:
			nums, err := getAllExtensionNumbersOfType(messageRequest.AllExtensionNumbersOfType)
			if err != nil {
				response.MessageResponse = newNotFoundResponse(err)
			} else {
				response.MessageResponse = &reflectionv1alpha1.ServerReflectionResponse_AllExtensionNumbersResponse{
					AllExtensionNumbersResponse: &reflectionv1alpha1.ExtensionNumberResponse{
						BaseTypeName:    messageRequest.AllExtensionNumbersOfType,
						ExtensionNumber: nums,
					},
				}
			}
		case *reflectionv1alpha1.ServerReflectionRequest_ListServices:
			services := r.serviceNames()
			serviceResponses := make([]*reflectionv1alpha1.ServiceResponse, len(services))
			for i, name := range services {
				serviceResponses[i] = &reflectionv1alpha1.ServiceResponse{Name: name}
			}
			response.MessageResponse = &reflectionv1alpha1.ServerReflectionResponse_ListServicesResponse{
				ListServicesResponse: &reflectionv1alpha1.ListServiceResponse{Service: serviceResponses},
			}
		default:
			return NewError(CodeInvalidArgument, fmt.Errorf(
				"invalid MessageRequest: %v",
				request.MessageRequest,
			))
		}
		if err := stream.Send(response); err != nil {
			return err
		}
	}
}

func getFileByFilename(fname string, sent *fileDescriptorNameSet) ([][]byte, error) {
	fd, err := protoregistry.GlobalFiles.FindFileByPath(fname)
	if err != nil {
		return nil, err
	}
	return fileDescriptorWithDependencies(fd, sent)
}

func getFileContainingSymbol(fqn string, sent *fileDescriptorNameSet) ([][]byte, error) {
	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(fqn))
	if err != nil {
		return nil, err
	}
	fd := desc.ParentFile()
	if fd == nil {
		return nil, fmt.Errorf("no file for symbol %s", fqn)
	}
	return fileDescriptorWithDependencies(fd, sent)
}

func getFileContainingExtension(
	msgFQN string,
	extNumber int32,
	sent *fileDescriptorNameSet,
) ([][]byte, error) {
	extension, err := protoregistry.GlobalTypes.FindExtensionByNumber(
		protoreflect.FullName(msgFQN),
		protoreflect.FieldNumber(extNumber),
	)
	if err != nil {
		return nil, err
	}
	fd := extension.TypeDescriptor().ParentFile()
	if fd == nil {
		return nil, fmt.Errorf("no file for extension %d of message %s", extNumber, msgFQN)
	}
	return fileDescriptorWithDependencies(fd, sent)
}

func getAllExtensionNumbersOfType(fqn string) ([]int32, error) {
	nums := []int32{}
	name := protoreflect.FullName(fqn)
	protoregistry.GlobalTypes.RangeExtensionsByMessage(name, func(ext protoreflect.ExtensionType) bool {
		num := int32(ext.TypeDescriptor().Number())
		nums = append(nums, num)
		return true
	})
	sort.Slice(nums, func(i, j int) bool {
		return nums[i] < nums[j]
	})
	return nums, nil
}

func fileDescriptorWithDependencies(fd protoreflect.FileDescriptor, sent *fileDescriptorNameSet) ([][]byte, error) {
	r := make([][]byte, 0, 1)
	queue := []protoreflect.FileDescriptor{fd}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		if len(r) == 0 || !sent.Contains(curr) { // always send root fd
			// Mark as sent immediately. If we hit an error marshaling below, there's
			// no point trying again later.
			sent.Insert(curr)
			encoded, err := proto.Marshal(protodesc.ToFileDescriptorProto(curr))
			if err != nil {
				return nil, err
			}
			r = append(r, encoded)
		}
		imports := curr.Imports()
		for i := 0; i < imports.Len(); i++ {
			queue = append(queue, imports.Get(i).FileDescriptor)
		}
	}
	return r, nil
}

type fileDescriptorNameSet struct {
	names map[protoreflect.FullName]struct{}
}

func (s *fileDescriptorNameSet) Insert(fd protoreflect.FileDescriptor) {
	if s.names == nil {
		s.names = make(map[protoreflect.FullName]struct{}, 1)
	}
	s.names[fd.FullName()] = struct{}{}
}

func (s *fileDescriptorNameSet) Contains(fd protoreflect.FileDescriptor) bool {
	_, ok := s.names[fd.FullName()]
	return ok
}

func newNotFoundResponse(err error) *reflectionv1alpha1.ServerReflectionResponse_ErrorResponse {
	return &reflectionv1alpha1.ServerReflectionResponse_ErrorResponse{
		ErrorResponse: &reflectionv1alpha1.ErrorResponse{
			ErrorCode:    int32(CodeNotFound),
			ErrorMessage: err.Error(),
		},
	}
}
