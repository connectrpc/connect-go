package rerpc

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	rpb "github.com/akshayjshah/rerpc/internal/reflectionpb/v1alpha"
)

// A Registrar collects information to support gRPC server reflection
// when building handlers.
//
// A registrar is a valid HandlerOption.
type Registrar struct {
	mu       sync.RWMutex
	services map[string]struct{}
}

// NewRegistrar constructs an empty Registrar.
func NewRegistrar() *Registrar {
	return &Registrar{services: make(map[string]struct{})}
}

// Services returns the fully-qualified names of the registered protobuf
// services. The returned slice is a copy, so it's safe for callers to modify.
func (r *Registrar) Services() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.services))
	for n, _ := range r.services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// IsRegistered checks whether a fully-qualified protobuf service name is
// registered.
func (r *Registrar) IsRegistered(service string) bool {
	r.mu.RLock()
	_, ok := r.services[service]
	r.mu.RUnlock()
	return ok
}

func (r *Registrar) register(method string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := strings.LastIndexByte(method, '.')
	if i <= 0 {
		// protobuf method names should always be at least service.method -
		// if we can't find a period, the name is malformed.
		return
	}
	r.services[method[:i]] = struct{}{}
}

func (r *Registrar) apply(cfg *handlerCfg) {
	cfg.Registrar = r
}

// NewReflectionHandler uses the information in the supplied Registrar to
// construct an HTTP handler for gRPC's server reflection API. It returns the
// HTTP handler and the correct path on which to mount it.
//
// Note that because the reflection API requires bidirectional streaming, the
// returned handler only supports gRPC over HTTP/2 (i.e., it doesn't support
// JSON or HTTP/1.x).
//
// While the reflection API makes tools like grpcurl convenient, keep in mind
// that it exposes every protobuf package compiled into your binary. Think
// twice before exposing it outside your organization.
//
// For more information, see
//   https://github.com/grpc/grpc-go/blob/master/Documentation/server-reflection-tutorial.md
//   https://github.com/grpc/grpc/blob/master/doc/server-reflection.md
//   https://github.com/fullstorydev/grpcurl
func NewReflectionHandler(reg *Registrar) (string, http.Handler) {
	const fqn = "grpc.reflection.v1alpha.ServerReflection.ServerReflectionInfo"
	reg.register(fqn)
	raw := &rawReflectionHandler{*reg}
	h := NewHandler(
		fqn,
		nil,                       // no unary implementation
		HandlerSupportJSON(false), // no JSON streaming
	)
	h.rawGRPC = raw.rawGRPC
	httpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Serve(w, r, nil)
	})
	return "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo", httpHandler
}

type rawReflectionHandler struct {
	reg Registrar
}

func (rh *rawReflectionHandler) rawGRPC(w http.ResponseWriter, r *http.Request, requestCompression, responseCompression string) {
	if r.ProtoMajor < 2 {
		w.WriteHeader(http.StatusHTTPVersionNotSupported)
		io.WriteString(w, "bidirectional streaming requires HTTP/2")
		return
	}
	for {
		var req rpb.ServerReflectionRequest
		if err := unmarshalLPM(r.Body, &req, requestCompression, 0); err != nil && errors.Is(err, io.EOF) {
			writeErrorGRPC(w, nil)
			return
		} else if err != nil {
			writeErrorGRPC(w, errorf(CodeUnknown, "can't unmarshal protobuf"))
			return
		}

		res, serr := rh.serve(&req)
		if serr != nil {
			writeErrorGRPC(w, serr)
			return
		}

		if err := marshalLPM(w, res, responseCompression, 0); err != nil {
			writeErrorGRPC(w, errorf(CodeUnknown, "can't marshal protobuf"))
			return
		}

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (rh *rawReflectionHandler) serve(req *rpb.ServerReflectionRequest) (*rpb.ServerReflectionResponse, *Error) {
	// The grpc-go implementation of server reflection uses the APIs from
	// github.com/google/protobuf, which makes the logic fairly complex. The new
	// google.golang.org/protobuf/reflect/protoregistry exposes a higher-level
	// API that we'll use here.
	//
	// Note that the server reflection API sends file descriptors as uncompressed
	// proto-serialized bytes.
	fileDescriptorsSent := &fdset{}
	_ = fileDescriptorsSent // FIXME
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
					ErrorCode:    int32(CodeNotFound),
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
					ErrorCode:    int32(CodeNotFound),
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
					ErrorCode:    int32(CodeNotFound),
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
					ErrorCode:    int32(CodeNotFound),
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
		services := rh.reg.Services()
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
		return nil, errorf(CodeInvalidArgument, "invalid MessageRequest: %v", req.MessageRequest)
	}
	return res, nil
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
