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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestGRPCHandlerSender(t *testing.T) {
	newSender := func(web bool) *handlerSender {
		responseWriter := httptest.NewRecorder()
		protobufCodec := &protoBinaryCodec{}
		return &handlerSender{
			web: web,
			marshaler: marshaler{
				writer: responseWriter,
				codec:  protobufCodec,
			},
			protobuf: protobufCodec,
			writer:   responseWriter,
			header:   make(http.Header),
			trailer:  make(http.Header),
		}
	}
	t.Run("web", func(t *testing.T) {
		testGRPCHandlerSenderMetadata(t, newSender(true))
	})
	t.Run("http2", func(t *testing.T) {
		testGRPCHandlerSenderMetadata(t, newSender(false))
	})
}

func testGRPCHandlerSenderMetadata(t *testing.T, sender Sender) {
	// Closing the sender shouldn't unpredictably mutate user-visible headers or
	// trailers.
	expectHeaders := sender.Header().Clone()
	expectTrailers := sender.Trailer().Clone()
	sender.Close(NewError(CodeUnavailable, errors.New("oh no")))
	if diff := cmp.Diff(expectHeaders, sender.Header()); diff != "" {
		t.Errorf("headers changed:\n%s", diff)
	}
	if diff := cmp.Diff(expectTrailers, sender.Trailer()); diff != "" {
		t.Errorf("trailers changed:\n%s", diff)
	}
}
