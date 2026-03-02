// Copyright 2021-2025 The Connect Authors
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
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/internal/assert"
	"github.com/google/go-cmp/cmp"
)

func TestGRPCHandlerSender(t *testing.T) {
	t.Parallel()
	newConn := func(web bool) *grpcHandlerConn {
		responseWriter := httptest.NewRecorder()
		protobufCodec := &protoBinaryCodec{}
		bufferPool := newBufferPool()
		request, err := http.NewRequest(
			http.MethodPost,
			"https://demo.example.com",
			strings.NewReader(""),
		)
		assert.Nil(t, err)
		return &grpcHandlerConn{
			spec:       Spec{},
			web:        web,
			bufferPool: bufferPool,
			protobuf:   protobufCodec,
			marshaler: grpcMarshaler{
				envelopeWriter: envelopeWriter{
					sender:     writeSender{writer: responseWriter},
					codec:      protobufCodec,
					bufferPool: bufferPool,
				},
			},
			responseWriter:  responseWriter,
			responseHeader:  make(http.Header),
			responseTrailer: make(http.Header),
			request:         request,
			unmarshaler: grpcUnmarshaler{
				envelopeReader: envelopeReader{
					reader:     request.Body,
					codec:      protobufCodec,
					bufferPool: bufferPool,
				},
			},
		}
	}
	t.Run("web", func(t *testing.T) {
		t.Parallel()
		testGRPCHandlerConnMetadata(t, newConn(true))
	})
	t.Run("http2", func(t *testing.T) {
		t.Parallel()
		testGRPCHandlerConnMetadata(t, newConn(false))
	})
}

func testGRPCHandlerConnMetadata(t *testing.T, conn handlerConnCloser) {
	// Closing the sender shouldn't unpredictably mutate user-visible headers or
	// trailers.
	t.Helper()
	expectHeaders := conn.ResponseHeader().Clone()
	expectTrailers := conn.ResponseTrailer().Clone()
	conn.Close(NewError(CodeUnavailable, errors.New("oh no")))
	if diff := cmp.Diff(expectHeaders, conn.ResponseHeader()); diff != "" {
		t.Errorf("headers changed:\n%s", diff)
	}
	gotTrailers := conn.ResponseTrailer()
	if diff := cmp.Diff(expectTrailers, gotTrailers); diff != "" {
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
	timeout := grpcEncodeTimeout(time.Hour + time.Second)
	assert.Equal(t, timeout, "3601000m") // NB, m is milliseconds

	// overflow and underflow
	timeout = grpcEncodeTimeout(time.Duration(math.MaxInt64))
	assert.Equal(t, timeout, "2562047H")
	timeout = grpcEncodeTimeout(-1)
	assert.Equal(t, timeout, "0n")
	timeout = grpcEncodeTimeout(-1 * time.Hour)
	assert.Equal(t, timeout, "0n")

	// unit conversions
	const eightDigitsNanos = 99999999 * time.Nanosecond
	timeout = grpcEncodeTimeout(eightDigitsNanos) // shouldn't need unit conversion
	assert.Equal(t, timeout, "99999999n")
	timeout = grpcEncodeTimeout(eightDigitsNanos + 1) // 9 digits, convert to micros
	assert.Equal(t, timeout, "100000u")

	// rounding
	timeout = grpcEncodeTimeout(10*time.Millisecond + 1) // shouldn't round
	assert.Equal(t, timeout, "10000001n")
	timeout = grpcEncodeTimeout(10*time.Second + 1) // should round down
	assert.Equal(t, timeout, "10000000u")
}

func TestGRPCPercentEncodingQuick(t *testing.T) {
	t.Parallel()
	roundtrip := func(input string) bool {
		if !utf8.ValidString(input) {
			return true
		}
		encoded := grpcPercentEncode(input)
		decoded, err := grpcPercentDecode(encoded)
		return err == nil && decoded == input
	}
	if err := quick.Check(roundtrip, nil /* config */); err != nil {
		t.Error(err)
	}
}

func TestGRPCPercentEncoding(t *testing.T) {
	t.Parallel()
	roundtrip := func(input string) {
		assert.True(t, utf8.ValidString(input), assert.Sprintf("input invalid UTF-8"))
		encoded := grpcPercentEncode(input)
		t.Logf("%q encoded as %q", input, encoded)
		decoded, err := grpcPercentDecode(encoded)
		assert.Nil(t, err)
		assert.Equal(t, decoded, input)
	}

	roundtrip("foo")
	roundtrip("foo bar")
	roundtrip(`foo%bar`)
	roundtrip("fiancée")
}

func TestGRPCWebTrailerMarshalling(t *testing.T) {
	t.Parallel()
	responseWriter := httptest.NewRecorder()
	marshaler := grpcMarshaler{
		envelopeWriter: envelopeWriter{
			sender:     writeSender{writer: responseWriter},
			bufferPool: newBufferPool(),
		},
	}
	trailer := http.Header{}
	trailer.Add("grpc-status", "0")
	trailer.Add("Grpc-Message", "Foo")
	trailer.Add("User-Provided", "bar")
	err := marshaler.MarshalWebTrailers(trailer)
	assert.Nil(t, err)
	responseWriter.Body.Next(5) // skip flags and message length
	marshalled := responseWriter.Body.String()
	assert.Equal(t, marshalled, "grpc-message: Foo\r\ngrpc-status: 0\r\nuser-provided: bar\r\n")
}

func BenchmarkGRPCPercentEncoding(b *testing.B) {
	input := "Hello, 世界"
	want := "Hello, %E4%B8%96%E7%95%8C"
	b.ReportAllocs()
	for b.Loop() {
		got := grpcPercentEncode(input)
		if got != want {
			b.Fatalf("grpcPercentEncode(%q) = %s, want %s", input, got, want)
		}
	}
}

func BenchmarkGRPCPercentDecoding(b *testing.B) {
	input := "Hello, %E4%B8%96%E7%95%8C"
	want := "Hello, 世界"
	b.ReportAllocs()
	for b.Loop() {
		got, _ := grpcPercentDecode(input)
		if got != want {
			b.Fatalf("grpcPercentDecode(%q) = %s, want %s", input, got, want)
		}
	}
}

func BenchmarkGRPCTimeoutEncoding(b *testing.B) {
	input := time.Second * 45
	want := "45000000u"
	b.ReportAllocs()
	for b.Loop() {
		got := grpcEncodeTimeout(input)
		if got != want {
			b.Fatalf("grpcEncodeTimeout(%q) = %s, want %s", input, got, want)
		}
	}
}

func TestGRPCValidateResponseContentType(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		web                 bool
		codecName           string
		responseContentType string
		expectCode          Code
	}{
		// Allowed content-types
		{
			codecName:           codecNameProto,
			responseContentType: "application/grpc",
		},
		{
			codecName:           codecNameProto,
			responseContentType: "application/grpc+proto",
		},
		{
			codecName:           codecNameJSON,
			responseContentType: "application/grpc+json",
		},
		{
			codecName:           codecNameProto,
			web:                 true,
			responseContentType: "application/grpc-web",
		},
		{
			codecName:           codecNameProto,
			web:                 true,
			responseContentType: "application/grpc-web+proto",
		},
		{
			codecName:           codecNameJSON,
			web:                 true,
			responseContentType: "application/grpc-web+json",
		},
		// Mismatched response codec
		{
			codecName:           codecNameProto,
			responseContentType: "application/grpc+json",
			expectCode:          CodeInternal,
		},
		{
			codecName:           codecNameJSON,
			responseContentType: "application/grpc",
			expectCode:          CodeInternal,
		},
		{
			codecName:           codecNameJSON,
			responseContentType: "application/grpc+proto",
			expectCode:          CodeInternal,
		},
		{
			codecName:           codecNameProto,
			web:                 true,
			responseContentType: "application/grpc-web+json",
			expectCode:          CodeInternal,
		},
		{
			codecName:           codecNameJSON,
			web:                 true,
			responseContentType: "application/grpc-web",
			expectCode:          CodeInternal,
		},
		{
			codecName:           codecNameJSON,
			web:                 true,
			responseContentType: "application/grpc-web+proto",
			expectCode:          CodeInternal,
		},
		// Disallowed content-types
		{
			codecName:           codecNameProto,
			responseContentType: "application/proto",
			expectCode:          CodeUnknown,
		},
		{
			codecName:           codecNameProto,
			responseContentType: "application/grpc-web",
			expectCode:          CodeUnknown,
		},
		{
			codecName:           codecNameProto,
			responseContentType: "application/grpc-web+proto",
			expectCode:          CodeUnknown,
		},
		{
			codecName:           codecNameJSON,
			responseContentType: "application/json",
			expectCode:          CodeUnknown,
		},
		{
			codecName:           codecNameJSON,
			responseContentType: "application/grpc-web+json",
			expectCode:          CodeUnknown,
		},
		{
			codecName:           codecNameProto,
			web:                 true,
			responseContentType: "application/proto",
			expectCode:          CodeUnknown,
		},
		{
			codecName:           codecNameProto,
			web:                 true,
			responseContentType: "application/grpc",
			expectCode:          CodeUnknown,
		},
		{
			codecName:           codecNameProto,
			web:                 true,
			responseContentType: "application/grpc+proto",
			expectCode:          CodeUnknown,
		},
		{
			codecName:           codecNameJSON,
			web:                 true,
			responseContentType: "application/json",
			expectCode:          CodeUnknown,
		},
		{
			codecName:           codecNameJSON,
			web:                 true,
			responseContentType: "application/grpc+json",
			expectCode:          CodeUnknown,
		},
		{
			codecName:           codecNameProto,
			responseContentType: "some/garbage",
			expectCode:          CodeUnknown,
		},
	}
	for _, testCase := range testCases {
		protocol := ProtocolGRPC
		if testCase.web {
			protocol = ProtocolGRPCWeb
		}
		testCaseName := fmt.Sprintf("%s_%s->%s", protocol, testCase.codecName, testCase.responseContentType)
		t.Run(testCaseName, func(t *testing.T) {
			t.Parallel()
			err := grpcValidateResponseContentType(
				testCase.web,
				testCase.codecName,
				testCase.responseContentType,
			)
			if testCase.expectCode == 0 {
				assert.Nil(t, err)
			} else if assert.NotNil(t, err) {
				assert.Equal(t, CodeOf(err), testCase.expectCode)
				assert.True(t, strings.Contains(err.Message(), fmt.Sprintf("invalid content-type: %q; expecting", testCase.responseContentType)))
			}
		})
	}
}
