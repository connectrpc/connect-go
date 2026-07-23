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

package internal

import (
	"fmt"

	conformancev1 "connectrpc.com/connect/v2/internal/conformance/internal/gen/connectrpc/conformance/v1"
	"google.golang.org/protobuf/types/known/emptypb"
)

type CancelTiming struct {
	BeforeCloseSend   *emptypb.Empty
	AfterCloseSendMs  int
	AfterNumResponses int
}

// GetCancelTiming evaluates a Cancel setting and returns a struct with the
// appropriate value set.
func GetCancelTiming(cancel *conformancev1.ClientCompatRequest_Cancel) (*CancelTiming, error) {
	var beforeCloseSend *emptypb.Empty
	afterCloseSendMs := -1
	afterNumResponses := -1
	if cancel != nil {
		switch cancelTiming := cancel.CancelTiming.(type) {
		case *conformancev1.ClientCompatRequest_Cancel_BeforeCloseSend:
			beforeCloseSend = cancelTiming.BeforeCloseSend
		case *conformancev1.ClientCompatRequest_Cancel_AfterCloseSendMs:
			afterCloseSendMs = int(cancelTiming.AfterCloseSendMs)
		case *conformancev1.ClientCompatRequest_Cancel_AfterNumResponses:
			afterNumResponses = int(cancelTiming.AfterNumResponses)
		case nil:
			// If cancel is non-nil, but none of timing values are set, it should
			// be treated as if afterCloseSendMs was set to 0
			afterCloseSendMs = 0
		default:
			return nil, fmt.Errorf("provided CancelTiming has an unexpected type %T", cancelTiming)
		}
	}
	return &CancelTiming{
		BeforeCloseSend:   beforeCloseSend,
		AfterCloseSendMs:  afterCloseSendMs,
		AfterNumResponses: afterNumResponses,
	}, nil
}
