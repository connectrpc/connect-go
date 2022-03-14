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

// Package grpcreflect offers support for gRPC's server reflection API. Server
// reflection is optional, but makes debugging tools like grpcurl much more
// convenient.
package grpcreflect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/bufbuild/connect"
	reflectionv1alpha1 "github.com/bufbuild/connect/internal/gen/go/connectext/grpc/reflection/v1alpha"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// NewHandler constructs a simple implementation of the gRPC server reflection
// API. It returns an HTTP handler and the path on which to mount it. Note that
// because the reflection API requires bidirectional streaming, the returned
// handler doesn't support gRPC-Web.
//
// This implementation is simple: the list of services available for reflection
// is fixed at build time, and the protobuf type information is pulled from the
// package-global registries in google.golang.org/protobuf.
//
// To build a reflection handler with a dynamic list of services or a different
// source of protobuf type information, construct a Reflector directly and use
// NewCustomHandler.
func NewHandler(
	services []string,
	options ...connect.HandlerOption) (string, http.Handler) {
	namer := &staticNames{names: services}
	return NewCustomHandler(NewReflector(namer), options...)
}

// NewCustomHandler constructs an HTTP handler for gRPC's server reflection
// API. It returns the path on which to mount the handler and the handler
// itself. Note that because the reflection API requires bidirectional
// streaming, the returned handler doesn't support gRPC-Web.
func NewCustomHandler(reflector *Reflector, options ...connect.HandlerOption) (string, http.Handler) {
	const serviceName = "/grpc.reflection.v1alpha.ServerReflection/"
	return serviceName, connect.NewBidiStreamHandler(
		serviceName+"ServerReflectionInfo",
		reflector.serverReflectionInfo,
		options...,
	)
}

// Reflectors implement the underlying logic for gRPC's protobuf server
// reflection. They're configurable, so they can support simple, process-local
// reflection or more complex proxying.
//
// Keep in mind that by default, Reflectors expose every protobuf type and
// extension compiled into your binary. Think twice before exposing them
// outside your organization.
//
// For more information, see:
// https://github.com/grpc/grpc-go/blob/master/Documentation/server-reflection-tutorial.md,
// https://github.com/grpc/grpc/blob/master/doc/server-reflection.md, and
// https://github.com/fullstorydev/grpcurl.
type Reflector struct {
	namer              Namer
	extensionResolver  ExtensionResolver
	descriptorResolver protodesc.Resolver
}

// NewReflector constructs a Reflector. To build a simple Reflector that
// supports a static list of services using information from the package-global
// protobuf registry, use NewStaticNamer and the default options.
func NewReflector(namer Namer, options ...Option) *Reflector {
	reflector := &Reflector{
		namer:              namer,
		extensionResolver:  protoregistry.GlobalTypes,
		descriptorResolver: protoregistry.GlobalFiles,
	}
	for _, option := range options {
		option.apply(reflector)
	}
	return reflector
}

// serverReflectionInfo implements the gRPC server reflection API.
func (r *Reflector) serverReflectionInfo(
	ctx context.Context,
	stream *connect.BidiStream[
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
		// The server reflection API sends file descriptors as uncompressed
		// proto-serialized bytes.
		response := &reflectionv1alpha1.ServerReflectionResponse{
			ValidHost:       request.Host,
			OriginalRequest: request,
		}
		switch messageRequest := request.MessageRequest.(type) {
		case *reflectionv1alpha1.ServerReflectionRequest_FileByFilename:
			data, err := r.getFileByFilename(messageRequest.FileByFilename, fileDescriptorsSent)
			if err != nil {
				response.MessageResponse = newNotFoundResponse(err)
			} else {
				response.MessageResponse = &reflectionv1alpha1.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &reflectionv1alpha1.FileDescriptorResponse{FileDescriptorProto: data},
				}
			}
		case *reflectionv1alpha1.ServerReflectionRequest_FileContainingSymbol:
			data, err := r.getFileContainingSymbol(
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
			data, err := r.getFileContainingExtension(msgFQN, extNumber, fileDescriptorsSent)
			if err != nil {
				response.MessageResponse = newNotFoundResponse(err)
			} else {
				response.MessageResponse = &reflectionv1alpha1.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &reflectionv1alpha1.FileDescriptorResponse{FileDescriptorProto: data},
				}
			}
		case *reflectionv1alpha1.ServerReflectionRequest_AllExtensionNumbersOfType:
			nums, err := r.getAllExtensionNumbersOfType(messageRequest.AllExtensionNumbersOfType)
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
			services := r.namer.Names()
			serviceResponses := make([]*reflectionv1alpha1.ServiceResponse, len(services))
			for i, name := range services {
				serviceResponses[i] = &reflectionv1alpha1.ServiceResponse{Name: name}
			}
			response.MessageResponse = &reflectionv1alpha1.ServerReflectionResponse_ListServicesResponse{
				ListServicesResponse: &reflectionv1alpha1.ListServiceResponse{Service: serviceResponses},
			}
		default:
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
				"invalid MessageRequest: %v",
				request.MessageRequest,
			))
		}
		if err := stream.Send(response); err != nil {
			return err
		}
	}
}

func (r *Reflector) getFileByFilename(fname string, sent *fileDescriptorNameSet) ([][]byte, error) {
	fd, err := r.descriptorResolver.FindFileByPath(fname)
	if err != nil {
		return nil, err
	}
	return fileDescriptorWithDependencies(fd, sent)
}

func (r *Reflector) getFileContainingSymbol(fqn string, sent *fileDescriptorNameSet) ([][]byte, error) {
	desc, err := r.descriptorResolver.FindDescriptorByName(protoreflect.FullName(fqn))
	if err != nil {
		return nil, err
	}
	fd := desc.ParentFile()
	if fd == nil {
		return nil, fmt.Errorf("no file for symbol %s", fqn)
	}
	return fileDescriptorWithDependencies(fd, sent)
}

func (r *Reflector) getFileContainingExtension(
	msgFQN string,
	extNumber int32,
	sent *fileDescriptorNameSet,
) ([][]byte, error) {
	extension, err := r.extensionResolver.FindExtensionByNumber(
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

func (r *Reflector) getAllExtensionNumbersOfType(fqn string) ([]int32, error) {
	nums := []int32{}
	name := protoreflect.FullName(fqn)
	r.extensionResolver.RangeExtensionsByMessage(name, func(ext protoreflect.ExtensionType) bool {
		num := int32(ext.TypeDescriptor().Number())
		nums = append(nums, num)
		return true
	})
	sort.Slice(nums, func(i, j int) bool {
		return nums[i] < nums[j]
	})
	return nums, nil
}

// A Namer lists the fully-qualified protobuf service names available for
// reflection. Namers must be safe to call concurrently.
type Namer interface {
	Names() []string
}

// An Option configures a Reflector.
type Option interface {
	apply(*Reflector)
}

// WithExtensionResolver sets the resolver used to find protobuf extensions. By
// default, Reflectors use protoregistry.GlobalTypes.
func WithExtensionResolver(resolver ExtensionResolver) Option {
	return &extensionResolverOption{resolver: resolver}
}

// WithExtensionResolver sets the resolver used to find protobuf type
// information (typically called a "descriptor"). By default, Reflectors use
// protoregistry.GlobalFiles.
func WithDescriptorResolver(resolver protodesc.Resolver) Option {
	return &descriptorResolverOption{resolver: resolver}
}

// An ExtensionResolver lets server reflection implementations query details
// about the registered protobuf extensions. protoregistry.GlobalTypes
// implements ExtensionResolver.
//
// ExtensionResolver implementations must be safe to call concurrently.
type ExtensionResolver interface {
	protoregistry.ExtensionTypeResolver

	RangeExtensionsByMessage(protoreflect.FullName, func(protoreflect.ExtensionType) bool)
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

func fileDescriptorWithDependencies(fd protoreflect.FileDescriptor, sent *fileDescriptorNameSet) ([][]byte, error) {
	results := make([][]byte, 0, 1)
	queue := []protoreflect.FileDescriptor{fd}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		if len(results) == 0 || !sent.Contains(curr) { // always send root fd
			// Mark as sent immediately. If we hit an error marshaling below, there's
			// no point trying again later.
			sent.Insert(curr)
			encoded, err := proto.Marshal(protodesc.ToFileDescriptorProto(curr))
			if err != nil {
				return nil, err
			}
			results = append(results, encoded)
		}
		imports := curr.Imports()
		for i := 0; i < imports.Len(); i++ {
			queue = append(queue, imports.Get(i).FileDescriptor)
		}
	}
	return results, nil
}

func newNotFoundResponse(err error) *reflectionv1alpha1.ServerReflectionResponse_ErrorResponse {
	return &reflectionv1alpha1.ServerReflectionResponse_ErrorResponse{
		ErrorResponse: &reflectionv1alpha1.ErrorResponse{
			ErrorCode:    int32(connect.CodeNotFound),
			ErrorMessage: err.Error(),
		},
	}
}

type extensionResolverOption struct {
	resolver ExtensionResolver
}

func (o *extensionResolverOption) apply(reflector *Reflector) {
	reflector.extensionResolver = o.resolver
}

type descriptorResolverOption struct {
	resolver protodesc.Resolver
}

func (o *descriptorResolverOption) apply(reflector *Reflector) {
	reflector.descriptorResolver = o.resolver
}

type staticNames struct {
	names []string
}

func (n *staticNames) Names() []string {
	return n.names
}
