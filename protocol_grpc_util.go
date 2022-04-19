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
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"

	statusv1 "github.com/bufbuild/connect-go/internal/gen/go/connectext/grpc/status/v1"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	typeDefaultGRPC       = "application/grpc"
	typeWebGRPC           = "application/grpc-web"
	typeDefaultGRPCPrefix = typeDefaultGRPC + "+"
	typeWebGRPCPrefix     = typeWebGRPC + "+"
)

// userAgent follows https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#user-agents.
//
//   While the protocol does not require a user-agent to function it is recommended
//   that clients provide a structured user-agent string that provides a basic
//   description of the calling library, version & platform to facilitate issue diagnosis
//   in heterogeneous environments. The following structure is recommended to library developers:
//
//   User-Agent → "grpc-" Language ?("-" Variant) "/" Version ?( " ("  *(AdditionalProperty ";") ")" )
func userAgent() string {
	return fmt.Sprintf("grpc-go-connect/%s (%s)", Version, runtime.Version())
}

func isCommaOrSpace(c rune) bool {
	return c == ',' || c == ' '
}

func acceptPostValue(web bool, codecs readOnlyCodecs) string {
	bare, prefix := typeDefaultGRPC, typeDefaultGRPCPrefix
	if web {
		bare, prefix = typeWebGRPC, typeWebGRPCPrefix
	}
	names := codecs.Names()
	for i, name := range names {
		names[i] = prefix + name
	}
	if codecs.Get(codecNameProto) != nil {
		names = append(names, bare)
	}
	return strings.Join(names, ",")
}

func codecFromContentType(web bool, contentType string) string {
	if (!web && contentType == typeDefaultGRPC) || (web && contentType == typeWebGRPC) {
		// implicitly protobuf
		return codecNameProto
	}
	prefix := typeDefaultGRPCPrefix
	if web {
		prefix = typeWebGRPCPrefix
	}
	if !strings.HasPrefix(contentType, prefix) {
		return ""
	}
	return strings.TrimPrefix(contentType, prefix)
}

func contentTypeFromCodecName(web bool, name string) string {
	if web {
		return typeWebGRPCPrefix + name
	}
	return typeDefaultGRPCPrefix + name
}

func grpcErrorToTrailer(bufferPool *bufferPool, trailer http.Header, protobuf Codec, err error) {
	const (
		statusKey  = "Grpc-Status"
		messageKey = "Grpc-Message"
		detailsKey = "Grpc-Status-Details-Bin"
	)
	if err == nil {
		trailer.Set(statusKey, "0") // zero is the gRPC OK status
		trailer.Set(messageKey, "")
		return
	}
	status, statusErr := statusFromError(err)
	if statusErr != nil {
		trailer.Set(
			statusKey,
			strconv.FormatInt(int64(CodeInternal), 10 /* base */),
		)
		trailer.Set(messageKey, statusErr.Error())
		return
	}
	code := strconv.Itoa(int(status.Code))
	bin, binErr := protobuf.Marshal(status)
	if binErr != nil {
		trailer.Set(
			statusKey,
			strconv.FormatInt(int64(CodeInternal), 10 /* base */),
		)
		trailer.Set(
			messageKey,
			fmt.Sprintf("marshal protobuf status: %v", binErr),
		)
		return
	}
	if connectErr, ok := asError(err); ok {
		mergeHeaders(trailer, connectErr.meta)
	}
	trailer.Set(statusKey, code)
	trailer.Set(messageKey, percentEncode(bufferPool, status.Message))
	trailer.Set(detailsKey, EncodeBinaryHeader(bin))
}

func statusFromError(err error) (*statusv1.Status, error) {
	status := &statusv1.Status{
		Code:    int32(CodeUnknown),
		Message: err.Error(),
	}
	if connectErr, ok := asError(err); ok {
		status.Code = int32(connectErr.Code())
		for _, detail := range connectErr.details {
			// If the detail is already a protobuf Any, we're golden.
			if anyProtoDetail, ok := detail.(*anypb.Any); ok {
				status.Details = append(status.Details, anyProtoDetail)
				continue
			}
			// Otherwise, we convert it to an Any.
			// TODO: Should we also attempt to delegate this to the detail by
			// attempting an upcast to interface{ AsAny() *anypb.Any }?
			anyProtoDetail, err := anypb.New(detail)
			if err != nil {
				return nil, fmt.Errorf(
					"can't create an *anypb.Any from %v (type %T): %w",
					detail, detail, err,
				)
			}
			status.Details = append(status.Details, anyProtoDetail)
		}
		if underlyingErr := connectErr.Unwrap(); underlyingErr != nil {
			status.Message = underlyingErr.Error() // don't repeat code
		}
	}
	return status, nil
}

func discard(reader io.Reader) error {
	if lr, ok := reader.(*io.LimitedReader); ok {
		_, err := io.Copy(io.Discard, lr)
		return err
	}
	// We don't want to get stuck throwing data away forever, so limit how much
	// we're willing to do here: at most, we'll copy 4 MiB.
	lr := &io.LimitedReader{R: reader, N: 1024 * 1024 * 4}
	_, err := io.Copy(io.Discard, lr)
	return err
}
