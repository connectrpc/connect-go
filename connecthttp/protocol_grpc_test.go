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

package connecthttp

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"github.com/google/go-cmp/cmp"
)

func TestGRPCHandlerSender(t *testing.T) {
	t.Parallel()
	newConn := func(web bool) *grpcHandlerConn {
		responseWriter := httptest.NewRecorder()
		protobufCodec := &connectproto.BinaryCodec{}
		request := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			"https://demo.example.com",
			strings.NewReader(""),
		)
		return &grpcHandlerConn{
			spec:     connect.Spec{},
			web:      web,
			protobuf: protobufCodec,
			marshaler: grpcMarshaler{
				envelopeWriter: envelopeWriter{
					sender: writeSender{writer: responseWriter},
					codec:  protobufCodec,
				},
			},
			responseWriter:  responseWriter,
			responseHeader:  make(http.Header),
			responseTrailer: make(http.Header),
			request:         request,
			unmarshaler: grpcUnmarshaler{
				envelopeReader: envelopeReader{
					reader: request.Body,
					codec:  protobufCodec,
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
	conn.Close(connect.NewError(connect.CodeUnavailable, "oh no"))
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
			sender: writeSender{writer: responseWriter},
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
		expectCode          connect.Code
	}{
		// Allowed content-types
		{
			codecName:           connect.CodecNameProto,
			responseContentType: "application/grpc",
		},
		{
			codecName:           connect.CodecNameProto,
			responseContentType: "application/grpc+proto",
		},
		{
			codecName:           connect.CodecNameJSON,
			responseContentType: "application/grpc+json",
		},
		{
			codecName:           connect.CodecNameProto,
			web:                 true,
			responseContentType: "application/grpc-web",
		},
		{
			codecName:           connect.CodecNameProto,
			web:                 true,
			responseContentType: "application/grpc-web+proto",
		},
		{
			codecName:           connect.CodecNameJSON,
			web:                 true,
			responseContentType: "application/grpc-web+json",
		},
		// Mismatched response codec
		{
			codecName:           connect.CodecNameProto,
			responseContentType: "application/grpc+json",
			expectCode:          connect.CodeInternal,
		},
		{
			codecName:           connect.CodecNameJSON,
			responseContentType: "application/grpc",
			expectCode:          connect.CodeInternal,
		},
		{
			codecName:           connect.CodecNameJSON,
			responseContentType: "application/grpc+proto",
			expectCode:          connect.CodeInternal,
		},
		{
			codecName:           connect.CodecNameProto,
			web:                 true,
			responseContentType: "application/grpc-web+json",
			expectCode:          connect.CodeInternal,
		},
		{
			codecName:           connect.CodecNameJSON,
			web:                 true,
			responseContentType: "application/grpc-web",
			expectCode:          connect.CodeInternal,
		},
		{
			codecName:           connect.CodecNameJSON,
			web:                 true,
			responseContentType: "application/grpc-web+proto",
			expectCode:          connect.CodeInternal,
		},
		// Disallowed content-types
		{
			codecName:           connect.CodecNameProto,
			responseContentType: "application/proto",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameProto,
			responseContentType: "application/grpc-web",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameProto,
			responseContentType: "application/grpc-web+proto",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameJSON,
			responseContentType: "application/json",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameJSON,
			responseContentType: "application/grpc-web+json",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameProto,
			web:                 true,
			responseContentType: "application/proto",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameProto,
			web:                 true,
			responseContentType: "application/grpc",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameProto,
			web:                 true,
			responseContentType: "application/grpc+proto",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameJSON,
			web:                 true,
			responseContentType: "application/json",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameJSON,
			web:                 true,
			responseContentType: "application/grpc+json",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameProto,
			responseContentType: "some/garbage",
			expectCode:          connect.CodeUnknown,
		},
	}
	for _, testCase := range testCases {
		protocol := connect.ProtocolNameGRPC
		if testCase.web {
			protocol = connect.ProtocolNameGRPCWeb
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
				assert.Equal(t, connect.CodeOf(err), testCase.expectCode)
				assert.True(t, strings.Contains(err.Message(), fmt.Sprintf("invalid content-type: %q; expecting", testCase.responseContentType)))
			}
		})
	}
}

func TestGRPCErrorToTrailerDetailRoundTrip(t *testing.T) {
	t.Parallel()
	protobufCodec := &connectproto.BinaryCodec{}
	detail, err := connectproto.NewErrorDetail(&pingv1.PingResponse{Number: 42})
	assert.Nil(t, err)
	rpcErr := connect.NewError(connect.CodeInvalidArgument, "validation failed").
		WithDetail(detail)

	trailer := make(http.Header)
	grpcErrorToTrailer(t.Context(), trailer, protobufCodec, rpcErr)
	assert.Equal(t, trailer.Get(grpcHeaderStatus), strconv.Itoa(int(connect.CodeInvalidArgument)))
	assert.NotEqual(t, trailer.Get(grpcHeaderDetails), "")

	gotErr := grpcErrorForTrailer(t.Context(), protobufCodec, trailer)
	assert.NotNil(t, gotErr)
	assert.Equal(t, gotErr.Code(), connect.CodeInvalidArgument)
	details := gotErr.Details()
	if assert.Equal(t, len(details), 1) {
		assert.Equal(t, details[0].Type, detail.Type)
		assert.Equal(t, details[0].Value, detail.Value)
	}
}
