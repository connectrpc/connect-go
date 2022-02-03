package connect

import (
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"

	"github.com/bufconnect/connect/codec"
	"github.com/bufconnect/connect/codec/protobuf"
)

const (
	typeDefaultGRPC       = "application/grpc"
	typeWebGRPC           = "application/grpc-web"
	typeDefaultGRPCPrefix = typeDefaultGRPC + "+"
	typeWebGRPCPrefix     = typeWebGRPC + "+"
	// gRPC protocol uses "proto" instead of "protobuf"
	grpcNameProto = "proto"
)

var userAgent = []string{fmt.Sprintf("grpc-go-connect/%s (%s)", Version, runtime.Version())}

func isCommaOrSpace(c rune) bool {
	return c == ',' || c == ' '
}

func acceptPostValue(web bool, codecs roCodecs) string {
	bare, prefix := typeDefaultGRPC, typeDefaultGRPCPrefix
	if web {
		bare, prefix = typeWebGRPC, typeWebGRPCPrefix
	}
	names := codecs.Names()
	for i, n := range names {
		if n == protobuf.NameBinary {
			n = grpcNameProto
		}
		names[i] = prefix + n
	}
	if codecs.Get(protobuf.NameBinary) != nil {
		names = append(names, bare)
	}
	return strings.Join(names, ",")
}

func codecFromContentType(web bool, ctype string) string {
	if (!web && ctype == typeDefaultGRPC) || (web && ctype == typeWebGRPC) {
		// implicitly protobuf
		return protobuf.NameBinary
	}
	prefix := typeDefaultGRPCPrefix
	if web {
		prefix = typeWebGRPCPrefix
	}
	if !strings.HasPrefix(ctype, prefix) {
		return ""
	}
	name := strings.TrimPrefix(ctype, prefix)
	if name == grpcNameProto {
		// normalize to our "protobuf"
		return protobuf.NameBinary
	}
	return name
}

func contentTypeFromCodecName(web bool, n string) string {
	// translate back to gRPC's "proto"
	if n == protobuf.NameBinary {
		n = grpcNameProto
	}
	if web {
		return typeWebGRPCPrefix + n
	}
	return typeDefaultGRPCPrefix + n
}

func grpcErrorToTrailer(trailer http.Header, protobuf codec.Codec, err error) error {
	if CodeOf(err) == CodeOK { // safe for nil errors
		trailer.Set("Grpc-Status", strconv.Itoa(int(CodeOK)))
		trailer.Set("Grpc-Message", "")
		trailer.Set("Grpc-Status-Details-Bin", "")
		return nil
	}
	s, statusError := statusFromError(err)
	if statusError != nil {
		return statusError
	}
	code := strconv.Itoa(int(s.Code))
	bin, err := protobuf.Marshal(s)
	if err != nil {
		trailer.Set("Grpc-Status", strconv.Itoa(int(CodeInternal)))
		trailer.Set("Grpc-Message", percentEncode("error marshaling protobuf status with code "+code))
		return Errorf(CodeInternal, "couldn't marshal protobuf status: %w", err)
	}
	trailer.Set("Grpc-Status", code)
	trailer.Set("Grpc-Message", percentEncode(s.Message))
	trailer.Set("Grpc-Status-Details-Bin", EncodeBinaryHeader(bin))
	return nil
}
