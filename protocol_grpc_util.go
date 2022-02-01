package connect

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/bufconnect/connect/codec/protobuf"
)

const (
	typeDefaultGRPC = "application/grpc"
	typeGRPCPrefix  = typeDefaultGRPC + "+"
	// gRPC protocol uses "proto" instead of "protobuf"
	grpcNameProto = "proto"
)

var userAgent = []string{fmt.Sprintf("grpc-go-connect/%s (%s)", Version, runtime.Version())}

func isCommaOrSpace(c rune) bool {
	return c == ',' || c == ' '
}

func acceptPostValue(codecs roCodecs) string {
	names := codecs.Names()
	for i, n := range names {
		if n == protobuf.NameBinary {
			n = "proto"
		}
		names[i] = "application/grpc+" + n
	}
	names = append(names, "application/grpc")
	return strings.Join(names, ",")
}

func codecFromContentType(ctype string) string {
	if ctype == typeDefaultGRPC {
		// implicitly protobuf
		return protobuf.NameBinary
	}
	if !strings.HasPrefix(ctype, typeGRPCPrefix) {
		return ""
	}
	name := strings.TrimPrefix(ctype, typeGRPCPrefix)
	if name == grpcNameProto {
		// normalize to our "protobuf"
		return protobuf.NameBinary
	}
	return name
}

func contentTypeFromCodecName(n string) string {
	// translate back to gRPC's "proto"
	if n == protobuf.NameBinary {
		n = grpcNameProto
	}
	return typeGRPCPrefix + n
}
