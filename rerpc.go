package rerpc

import (
	"fmt"
	"runtime"
)

// Version is the semantic version of the reRPC module.
const Version = "0.0.1"

// MaxHeaderBytes is 8KiB, gRPC's recommended maximum header size. To enforce
// this limit, set MaxHeaderBytes on your http.Server.
const MaxHeaderBytes = 1024 * 8

// ReRPC's supported HTTP Content-Types. Servers decide whether to use the gRPC
// or Twirp protocol based on the request's Content-Type. See the protocol
// documentation at https://rerpc.github.io for more information.
const (
	TypeDefaultGRPC = "application/grpc"
	TypeProtoGRPC   = "application/grpc+proto"
	TypeProtoTwirp  = "application/protobuf"
	TypeJSON        = "application/json"
)

// To actually put the type constants into headers or trailers, we need a
// slice. Rather than allocating a new one for each write, we can pre-allocate
// here.
var (
	typeDefaultGRPCSlice = []string{TypeDefaultGRPC}
	typeProtoGRPCSlice   = []string{TypeProtoGRPC}
	typeProtoTwirpSlice  = []string{TypeProtoTwirp}
	typeJSONSlice        = []string{TypeJSON}
)

// typeToSlice returns the correct, already-allocated slice for a content-type.
// It's used when setting headers. The caller must not mutate the returned
// slice.
func typeToSlice(t string) []string {
	switch t {
	case TypeDefaultGRPC:
		return typeDefaultGRPCSlice
	case TypeProtoGRPC:
		return typeProtoGRPCSlice
	case TypeProtoTwirp:
		return typeProtoTwirpSlice
	case TypeJSON:
		return typeJSONSlice
	default:
		return []string{t}
	}
}

// ReRPC's supported compression methods.
const (
	CompressionIdentity = "identity"
	CompressionGzip     = "gzip"
)

// Similar to the content-type constants, we want to pre-allocate these slices
// to use when writing headers.
var (
	compressionIdentitySlice = []string{CompressionIdentity}
	compressionGzipSlice     = []string{CompressionGzip}
)

// compressionToSlice returns the correct, pre-allocated slice for a
// compression algorithm. It's used when setting headers. The caller must not
// mutate the returned slice.
func compressionToSlice(c string) []string {
	switch c {
	case "", CompressionIdentity:
		return compressionIdentitySlice
	case CompressionGzip:
		return compressionGzipSlice
	default:
		return []string{c}
	}
}

// StreamType describes whether the client, server, neither, or both is
// streaming.
type StreamType uint8

const (
	StreamTypeUnary         StreamType = 0b00
	StreamTypeClient                   = 0b01
	StreamTypeServer                   = 0b10
	StreamTypeBidirectional            = StreamTypeClient | StreamTypeServer
)

// These constants are used in compile-time handshakes with reRPC's generated
// code.
const (
	SupportsCodeGenV0 = iota
)

var (
	userAgent      = fmt.Sprintf("grpc-go-rerpc/%s (%s)", Version, runtime.Version())
	userAgentSlice = []string{userAgent}
)
