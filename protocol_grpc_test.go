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
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/quick"
	"time"

	"github.com/bufbuild/connect-go/internal/assert"
	"github.com/google/go-cmp/cmp"
)

func TestGRPCHandlerSender(t *testing.T) {
	t.Parallel()
	newSender := func(web bool) *grpcHandlerSender {
		responseWriter := httptest.NewRecorder()
		protobufCodec := &protoBinaryCodec{}
		bufferPool := newBufferPool()
		return &grpcHandlerSender{
			web: web,
			marshaler: grpcMarshaler{
				envelopeWriter: envelopeWriter{
					writer:     responseWriter,
					codec:      protobufCodec,
					bufferPool: bufferPool,
				},
			},
			protobuf:   protobufCodec,
			writer:     responseWriter,
			header:     make(http.Header),
			trailer:    make(http.Header),
			bufferPool: bufferPool,
		}
	}
	t.Run("web", func(t *testing.T) {
		t.Parallel()
		testGRPCHandlerSenderMetadata(t, newSender(true))
	})
	t.Run("http2", func(t *testing.T) {
		t.Parallel()
		testGRPCHandlerSenderMetadata(t, newSender(false))
	})
}

func testGRPCHandlerSenderMetadata(t *testing.T, sender Sender) {
	// Closing the sender shouldn't unpredictably mutate user-visible headers or
	// trailers.
	t.Helper()
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

func TestGRPCParseTimeout(t *testing.T) {
	t.Parallel()
	_, err := grpcParseTimeout("")
	assert.True(t, errors.Is(err, errNoTimeout))

	_, err = grpcParseTimeout("foo")
	assert.NotNil(t, err)
	_, err = grpcParseTimeout("12xS")
	assert.NotNil(t, err)
	_, err = grpcParseTimeout("999999999n") // 9 digits
	assert.NotNil(t, err)
	assert.False(t, errors.Is(err, errNoTimeout))
	_, err = grpcParseTimeout("99999999H") // 8 digits but overflows time.Duration
	assert.True(t, errors.Is(err, errNoTimeout))

	duration, err := grpcParseTimeout("45S")
	assert.Nil(t, err)
	assert.Equal(t, duration, 45*time.Second)

	const long = "99999999S"
	duration, err = grpcParseTimeout(long) // 8 digits, shouldn't overflow
	assert.Nil(t, err)
	assert.Equal(t, duration, 99999999*time.Second)
}

func TestGRPCEncodeTimeout(t *testing.T) {
	t.Parallel()
	timeout, err := grpcEncodeTimeout(time.Hour + time.Second)
	assert.Nil(t, err)
	assert.Equal(t, timeout, "3601000m")
	timeout, err = grpcEncodeTimeout(time.Duration(math.MaxInt64))
	assert.Nil(t, err)
	assert.Equal(t, timeout, "2562047H")
	timeout, err = grpcEncodeTimeout(-1 * time.Hour)
	assert.Nil(t, err)
	assert.Equal(t, timeout, "0n")
}

func TestGRPCEncodeTimeoutQuick(t *testing.T) {
	t.Parallel()
	// Ensure that the error case is actually unreachable.
	encode := func(d time.Duration) bool {
		_, err := grpcEncodeTimeout(d)
		return err == nil
	}
	if err := quick.Check(encode, nil); err != nil {
		t.Error(err)
	}
}
