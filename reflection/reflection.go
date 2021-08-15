// Package reflection offers support for gRPC's server reflection API. If you
// add reflection support to your gRPC server, many developer tools (including
// cURL replacements like grpcurl) become much more convenient.
package reflection

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/rerpc/rerpc"
	rpb "github.com/rerpc/rerpc/internal/reflection/v1alpha1"
)

// Registrar lists all registered protobuf services. The returned names must be
// fully qualified (e.g., "acme.foo.v1.FooService").
//
// A *rerpc.Registrar implements this interface.
type Registrar interface {
	Services() []string // returns fully-qualified protobuf services names
}

// NewHandler uses the information in the supplied Registrar to construct an
// HTTP handler for gRPC's server reflection API. It returns the HTTP handler
// and the correct path on which to mount it.
//
// Note that because the reflection API requires bidirectional streaming, the
// returned handler only supports gRPC over HTTP/2 (i.e., it doesn't support
// Twirp). Keep in mind that the reflection service exposes every protobuf
// package compiled into your binary - think twice before exposing it outside
// your organization.
//
// For more information, see:
// https://github.com/grpc/grpc-go/blob/master/Documentation/server-reflection-tutorial.md,
// https://github.com/grpc/grpc/blob/master/doc/server-reflection.md, and
// https://github.com/fullstorydev/grpcurl.
func NewHandler(reg Registrar, opts ...rerpc.HandlerOption) (string, *http.ServeMux) {
	const pkg = "grpc.reflection.v1alpha"
	const service = "ServerReflection"
	opts = append(opts, rerpc.OverrideProtobufTypes(pkg, service))
	return rpb.NewServerReflectionHandlerReRPC(&server{reg: reg}, opts...)
}

type server struct {
	rpb.UnimplementedServerReflectionReRPC

	reg Registrar
}

func (rs *server) ServerReflectionInfo(ctx context.Context, stream *rpb.ServerReflectionReRPC_ServerReflectionInfo) error {
	fileDescriptorsSent := &fdset{}
	for {
		req, err := stream.Receive()
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
		res := &rpb.ServerReflectionResponse{
			ValidHost:       req.Host,
			OriginalRequest: req,
		}
		switch mr := req.MessageRequest.(type) {
		case *rpb.ServerReflectionRequest_FileByFilename:
			b, err := getFileByFilename(mr.FileByFilename, fileDescriptorsSent)
			if err != nil {
				res.MessageResponse = &rpb.ServerReflectionResponse_ErrorResponse{
					ErrorResponse: &rpb.ErrorResponse{
						ErrorCode:    int32(rerpc.CodeNotFound),
						ErrorMessage: err.Error(),
					},
				}
			} else {
				res.MessageResponse = &rpb.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &rpb.FileDescriptorResponse{FileDescriptorProto: b},
				}
			}
		case *rpb.ServerReflectionRequest_FileContainingSymbol:
			b, err := getFileContainingSymbol(mr.FileContainingSymbol, fileDescriptorsSent)
			if err != nil {
				res.MessageResponse = &rpb.ServerReflectionResponse_ErrorResponse{
					ErrorResponse: &rpb.ErrorResponse{
						ErrorCode:    int32(rerpc.CodeNotFound),
						ErrorMessage: err.Error(),
					},
				}
			} else {
				res.MessageResponse = &rpb.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &rpb.FileDescriptorResponse{FileDescriptorProto: b},
				}
			}
		case *rpb.ServerReflectionRequest_FileContainingExtension:
			msgFQN := mr.FileContainingExtension.ContainingType
			ext := mr.FileContainingExtension.ExtensionNumber
			b, err := getFileContainingExtension(msgFQN, ext, fileDescriptorsSent)
			if err != nil {
				res.MessageResponse = &rpb.ServerReflectionResponse_ErrorResponse{
					ErrorResponse: &rpb.ErrorResponse{
						ErrorCode:    int32(rerpc.CodeNotFound),
						ErrorMessage: err.Error(),
					},
				}
			} else {
				res.MessageResponse = &rpb.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &rpb.FileDescriptorResponse{FileDescriptorProto: b},
				}
			}
		case *rpb.ServerReflectionRequest_AllExtensionNumbersOfType:
			nums, err := getAllExtensionNumbersOfType(mr.AllExtensionNumbersOfType)
			if err != nil {
				res.MessageResponse = &rpb.ServerReflectionResponse_ErrorResponse{
					ErrorResponse: &rpb.ErrorResponse{
						ErrorCode:    int32(rerpc.CodeNotFound),
						ErrorMessage: err.Error(),
					},
				}
			} else {
				res.MessageResponse = &rpb.ServerReflectionResponse_AllExtensionNumbersResponse{
					AllExtensionNumbersResponse: &rpb.ExtensionNumberResponse{
						BaseTypeName:    mr.AllExtensionNumbersOfType,
						ExtensionNumber: nums,
					},
				}
			}
		case *rpb.ServerReflectionRequest_ListServices:
			services := rs.reg.Services()
			serviceResponses := make([]*rpb.ServiceResponse, len(services))
			for i, n := range services {
				serviceResponses[i] = &rpb.ServiceResponse{
					Name: n,
				}
			}
			res.MessageResponse = &rpb.ServerReflectionResponse_ListServicesResponse{
				ListServicesResponse: &rpb.ListServiceResponse{
					Service: serviceResponses,
				},
			}
		default:
			return rerpc.Errorf(rerpc.CodeInvalidArgument, "invalid MessageRequest: %v", req.MessageRequest)
		}
		if err := stream.Send(res); err != nil {
			return err
		}
	}
}

func getFileByFilename(fname string, sent *fdset) ([][]byte, error) {
	fd, err := protoregistry.GlobalFiles.FindFileByPath(fname)
	if err != nil {
		return nil, err
	}
	return fileDescriptorWithDependencies(fd, sent)
}

func getFileContainingSymbol(fqn string, sent *fdset) ([][]byte, error) {
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

func getFileContainingExtension(msgFQN string, ext int32, sent *fdset) ([][]byte, error) {
	extension, err := protoregistry.GlobalTypes.FindExtensionByNumber(
		protoreflect.FullName(msgFQN),
		protoreflect.FieldNumber(ext),
	)
	if err != nil {
		return nil, err
	}
	fd := extension.TypeDescriptor().ParentFile()
	if fd == nil {
		return nil, fmt.Errorf("no file for extension %d of message %s", ext, msgFQN)
	}
	return fileDescriptorWithDependencies(fd, sent)
}

func getAllExtensionNumbersOfType(fqn string) ([]int32, error) {
	nums := []int32{}
	name := protoreflect.FullName(fqn)
	protoregistry.GlobalTypes.RangeExtensionsByMessage(name, func(ext protoreflect.ExtensionType) bool {
		n := int32(ext.TypeDescriptor().Number())
		nums = append(nums, n)
		return true
	})
	sort.Slice(nums, func(i, j int) bool {
		return nums[i] < nums[j]
	})
	return nums, nil
}

func fileDescriptorWithDependencies(fd protoreflect.FileDescriptor, sent *fdset) ([][]byte, error) {
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

type fdset struct {
	names map[protoreflect.FullName]struct{}
}

func (s *fdset) Insert(fd protoreflect.FileDescriptor) {
	if s.names == nil {
		s.names = make(map[protoreflect.FullName]struct{}, 1)
	}
	s.names[fd.FullName()] = struct{}{}
}

func (s *fdset) Contains(fd protoreflect.FileDescriptor) bool {
	_, ok := s.names[fd.FullName()]
	return ok
}
