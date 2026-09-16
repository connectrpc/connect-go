// Copyright 2021-2026 The Connect Authors
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

package connectproto

import (
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const anyResolverPrefix = "type.googleapis.com/"

// NewErrorDetail packs msg into a [*connect.ErrorDetail]. An *anypb.Any's
// type and value are used as-is.
func NewErrorDetail(msg proto.Message) (*connect.ErrorDetail, error) {
	if anyMsg, ok := msg.(*anypb.Any); ok {
		return &connect.ErrorDetail{
			Type:  typeNameForURL(anyMsg.GetTypeUrl()),
			Value: anyMsg.GetValue(),
		}, nil
	}
	value, err := proto.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return &connect.ErrorDetail{
		Type:  string(msg.ProtoReflect().Descriptor().FullName()),
		Value: value,
	}, nil
}

// ErrorDetailToAny converts detail into an *anypb.Any.
func ErrorDetailToAny(detail *connect.ErrorDetail) *anypb.Any {
	typeURL := detail.Type
	if !strings.Contains(typeURL, "/") {
		typeURL = anyResolverPrefix + typeURL
	}
	return &anypb.Any{TypeUrl: typeURL, Value: detail.Value}
}

// UnmarshalErrorDetail decodes detail into a Protobuf message using the
// global type registry. Typically, callers use Go type assertions to cast
// from the proto.Message interface to concrete types.
func UnmarshalErrorDetail(detail *connect.ErrorDetail) (proto.Message, error) {
	return ErrorDetailToAny(detail).UnmarshalNew()
}

// typeNameForURL trims the type-URL prefix from an *anypb.Any type URL.
func typeNameForURL(url string) string {
	return url[strings.LastIndexByte(url, '/')+1:]
}
