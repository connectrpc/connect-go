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
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestConnectErrorDetailMarshaling(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name        string
		errorDetail proto.Message
		expectDebug any
	}{
		{
			name: "normal",
			errorDetail: &descriptorpb.FieldOptions{
				Deprecated: proto.Bool(true),
				Jstype:     descriptorpb.FieldOptions_JS_STRING.Enum(),
			},
			expectDebug: map[string]any{
				"deprecated": true,
				"jstype":     "JS_STRING",
			},
		},
		{
			name:        "well-known type with custom JSON",
			errorDetail: durationpb.New(time.Second),
			expectDebug: "1s", // special JS representation as duration string
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			errorDetail, err := connectproto.NewErrorDetail(testCase.errorDetail)
			assert.Nil(t, err)
			detail := (*connectWireDetail)(errorDetail)
			data, err := json.Marshal(detail)
			assert.Nil(t, err)
			t.Logf("marshaled error detail: %s", string(data))

			var unmarshaled connectWireDetail
			assert.Nil(t, json.Unmarshal(data, &unmarshaled))
			assert.Equal(t, unmarshaled.Type, errorDetail.Type)
			assert.Equal(t, unmarshaled.Value, errorDetail.Value)
			assert.True(t, len(unmarshaled.Debug) > 0)

			var extractDetails struct {
				Debug any `json:"debug"`
			}
			assert.Nil(t, json.Unmarshal(data, &extractDetails))
			assert.Equal(t, extractDetails.Debug, testCase.expectDebug)
		})
	}
}

func TestConnectErrorDetailMarshalingNoDescriptor(t *testing.T) {
	t.Parallel()
	raw := `{"type":"acme.user.v1.User","value":"DEADBEEF",` +
		`"debug":{"email":"someone@connectrpc.com"}}`
	var detail connectWireDetail
	assert.Nil(t, json.Unmarshal([]byte(raw), &detail))
	assert.Equal(t, detail.Type, "acme.user.v1.User")
	anyDetail := connectproto.ErrorDetailToAny((*connect.ErrorDetail)(&detail))
	assert.Equal(t, anyDetail.GetTypeUrl(), "type.googleapis.com/acme.user.v1.User")

	_, err := connectproto.UnmarshalErrorDetail((*connect.ErrorDetail)(&detail))
	assert.NotNil(t, err)
	assert.True(t, strings.HasSuffix(err.Error(), "not found"))

	// Re-serializing a decoded detail preserves the debug field without
	// descriptors.
	encoded, err := json.Marshal(&detail)
	assert.Nil(t, err)
	assert.Equal(t, string(encoded), raw)
}

func TestConnectEndOfResponseCanonicalTrailers(t *testing.T) {
	t.Parallel()

	buffer := bytes.Buffer{}

	endStreamMessage := connectEndStreamMessage{Trailer: make(http.Header)}
	endStreamMessage.Trailer["not-canonical-header"] = []string{"a"}
	endStreamMessage.Trailer["mixed-Canonical"] = []string{"b"}
	endStreamMessage.Trailer["Mixed-Canonical"] = []string{"b"}
	endStreamMessage.Trailer["Canonical-Header"] = []string{"c"}
	endStreamData, err := json.Marshal(endStreamMessage)
	assert.Nil(t, err)

	writer := envelopeWriter{
		sender: writeSender{writer: &buffer},
	}
	err = writer.Write(&envelope{
		Flags: connectFlagEnvelopeEndStream,
		Data:  bytes.NewBuffer(endStreamData),
	})
	assert.Nil(t, err)

	unmarshaler := connectStreamingUnmarshaler{
		envelopeReader: envelopeReader{
			ctx:    t.Context(),
			reader: &buffer,
		},
	}
	err = unmarshaler.Unmarshal(nil) // parameter won't be used
	assert.ErrorIs(t, err, errSpecialEnvelope)
	assert.Equal(t, unmarshaler.Trailer().Values("Not-Canonical-Header"), []string{"a"})
	assert.Equal(t, unmarshaler.Trailer().Values("Mixed-Canonical"), []string{"b", "b"})
	assert.Equal(t, unmarshaler.Trailer().Values("Canonical-Header"), []string{"c"})
}

func TestConnectValidateUnaryResponseContentType(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		codecName            string
		get                  bool
		statusCode           int
		responseContentType  string
		expectCode           connect.Code
		expectBadContentType bool
		expectNotModified    bool
	}{
		// Allowed content-types for OK responses.
		{
			codecName:           connect.CodecNameProto,
			statusCode:          http.StatusOK,
			responseContentType: "application/proto",
		},
		{
			codecName:           connect.CodecNameJSON,
			statusCode:          http.StatusOK,
			responseContentType: "application/json",
		},
		{
			codecName:           connect.CodecNameJSON,
			statusCode:          http.StatusOK,
			responseContentType: "application/json; charset=utf-8",
		},
		{
			codecName:           codecNameJSONCharsetUTF8,
			statusCode:          http.StatusOK,
			responseContentType: "application/json",
		},
		{
			codecName:           codecNameJSONCharsetUTF8,
			statusCode:          http.StatusOK,
			responseContentType: "application/json; charset=utf-8",
		},
		// Allowed content-types for error responses.
		{
			codecName:           connect.CodecNameProto,
			statusCode:          http.StatusNotFound,
			responseContentType: "application/json",
		},
		{
			codecName:           connect.CodecNameProto,
			statusCode:          http.StatusBadRequest,
			responseContentType: "application/json; charset=utf-8",
		},
		{
			codecName:           connect.CodecNameJSON,
			statusCode:          http.StatusInternalServerError,
			responseContentType: "application/json",
		},
		{
			codecName:           connect.CodecNameJSON,
			statusCode:          http.StatusPreconditionFailed,
			responseContentType: "application/json; charset=utf-8",
		},
		// 304 Not Modified for GET request gets a special error, regardless of content-type
		{
			codecName:           connect.CodecNameProto,
			get:                 true,
			statusCode:          http.StatusNotModified,
			responseContentType: "application/json",
			expectCode:          connect.CodeUnknown,
			expectNotModified:   true,
		},
		{
			codecName:           connect.CodecNameJSON,
			get:                 true,
			statusCode:          http.StatusNotModified,
			responseContentType: "application/json",
			expectCode:          connect.CodeUnknown,
			expectNotModified:   true,
		},
		// OK status, invalid content-type
		{
			codecName:            connect.CodecNameProto,
			statusCode:           http.StatusOK,
			responseContentType:  "application/proto; charset=utf-8",
			expectCode:           connect.CodeInternal,
			expectBadContentType: true,
		},
		{
			codecName:            connect.CodecNameProto,
			statusCode:           http.StatusOK,
			responseContentType:  "application/json",
			expectCode:           connect.CodeInternal,
			expectBadContentType: true,
		},
		{
			codecName:            connect.CodecNameJSON,
			statusCode:           http.StatusOK,
			responseContentType:  "application/proto",
			expectCode:           connect.CodeInternal,
			expectBadContentType: true,
		},
		{
			codecName:            connect.CodecNameJSON,
			statusCode:           http.StatusOK,
			responseContentType:  "some/garbage",
			expectCode:           connect.CodeUnknown, // doesn't even look like it could be connect protocol
			expectBadContentType: true,
		},
		// connect.Error status, invalid content-type, returns code based on HTTP status code
		{
			codecName:           connect.CodecNameProto,
			statusCode:          http.StatusNotFound,
			responseContentType: "application/proto",
			expectCode:          httpToCode(http.StatusNotFound),
		},
		{
			codecName:           connect.CodecNameJSON,
			statusCode:          http.StatusBadRequest,
			responseContentType: "some/garbage",
			expectCode:          httpToCode(http.StatusBadRequest),
		},
		{
			codecName:           connect.CodecNameJSON,
			statusCode:          http.StatusTooManyRequests,
			responseContentType: "some/garbage",
			expectCode:          httpToCode(http.StatusTooManyRequests),
		},
	}
	for _, testCase := range testCases {
		httpMethod := http.MethodPost
		if testCase.get {
			httpMethod = http.MethodGet
		}
		testCaseName := fmt.Sprintf("%s_%s->%d_%s", httpMethod, testCase.codecName, testCase.statusCode, testCase.responseContentType)
		t.Run(testCaseName, func(t *testing.T) {
			t.Parallel()
			err := connectValidateUnaryResponseContentType(
				testCase.codecName,
				httpMethod,
				testCase.statusCode,
				http.StatusText(testCase.statusCode),
				testCase.responseContentType,
			)
			if testCase.expectCode == 0 {
				assert.Nil(t, err)
			} else if assert.NotNil(t, err) {
				assert.Equal(t, connect.CodeOf(err), testCase.expectCode)
				switch {
				case testCase.expectNotModified:
					assert.ErrorIs(t, err, errNotModified)
				case testCase.expectBadContentType:
					assert.True(t, strings.Contains(err.Message(), fmt.Sprintf("invalid content-type: %q; expecting", testCase.responseContentType)))
				default:
					assert.Equal(t, err.Message(), http.StatusText(testCase.statusCode))
				}
			}
		})
	}
}

func TestConnectValidateStreamResponseContentType(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		codecName           string
		responseContentType string
		expectCode          connect.Code
	}{
		// Allowed content-types
		{
			codecName:           connect.CodecNameProto,
			responseContentType: "application/connect+proto",
		},
		{
			codecName:           connect.CodecNameJSON,
			responseContentType: "application/connect+json",
		},
		// Mismatched response codec
		{
			codecName:           connect.CodecNameProto,
			responseContentType: "application/connect+json",
			expectCode:          connect.CodeInternal,
		},
		{
			codecName:           connect.CodecNameJSON,
			responseContentType: "application/connect+proto",
			expectCode:          connect.CodeInternal,
		},
		// Disallowed content-types
		{
			codecName:           connect.CodecNameJSON,
			responseContentType: "application/connect+json; charset=utf-8",
			expectCode:          connect.CodeInternal, // *almost* looks right
		},
		{
			codecName:           connect.CodecNameProto,
			responseContentType: "application/proto",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameJSON,
			responseContentType: "application/json",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameJSON,
			responseContentType: "application/json; charset=utf-8",
			expectCode:          connect.CodeUnknown,
		},
		{
			codecName:           connect.CodecNameProto,
			responseContentType: "some/garbage",
			expectCode:          connect.CodeUnknown,
		},
	}
	for _, testCase := range testCases {
		testCaseName := fmt.Sprintf("%s->%s", testCase.codecName, testCase.responseContentType)
		t.Run(testCaseName, func(t *testing.T) {
			t.Parallel()
			err := connectValidateStreamResponseContentType(
				testCase.codecName,
				connect.StreamTypeServer,
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

func TestConnectUnaryGetURLQueryOrder(t *testing.T) {
	t.Parallel()
	const baseURL = "http://example.com/connect.ping.v1.PingService/Ping"
	newMarshaler := func(t *testing.T, codec connect.StableCodec, compressionName string) *connectUnaryRequestMarshaler {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL, http.NoBody)
		assert.Nil(t, err)
		return &connectUnaryRequestMarshaler{
			connectUnaryMarshaler: connectUnaryMarshaler{
				codec:           codec,
				compressionName: compressionName,
			},
			stableCodec: codec,
			duplexCall:  &duplexHTTPCall{request: req},
		}
	}
	jsonCodec := &connectproto.JSONCodec{}
	protoCodec := &connectproto.BinaryCodec{}
	testCases := []struct {
		name            string
		codec           connect.StableCodec
		data            []byte
		compressed      bool
		compressionName string
		expectRawQuery  string
	}{
		{
			name:           "binary uncompressed",
			codec:          protoCodec,
			data:           []byte{0x01, 0x02, 0x03},
			expectRawQuery: "connect=v1&base64=1&encoding=proto&message=AQID",
		},
		{
			name:            "binary compressed",
			codec:           protoCodec,
			data:            []byte{0x04, 0x05, 0x06},
			compressed:      true,
			compressionName: "gzip",
			expectRawQuery:  "connect=v1&base64=1&compression=gzip&encoding=proto&message=BAUG",
		},
		{
			name:           "text uncompressed",
			codec:          jsonCodec,
			data:           []byte(`{"value":"hi"}`),
			expectRawQuery: "connect=v1&encoding=json&message=%7B%22value%22%3A%22hi%22%7D",
		},
		{
			name:            "text compressed forces base64",
			codec:           jsonCodec,
			data:            []byte{0x07, 0x08, 0x09},
			compressed:      true,
			compressionName: "gzip",
			expectRawQuery:  "connect=v1&base64=1&compression=gzip&encoding=json&message=BwgJ",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			marshaler := newMarshaler(t, testCase.codec, testCase.compressionName)
			got := marshaler.buildGetURL(testCase.data, testCase.compressed)
			assert.Equal(t, got.RawQuery, testCase.expectRawQuery)
		})
	}
}

func TestConnectWireErrorDetailBestEffortDebug(t *testing.T) {
	t.Parallel()
	// Packing failures surface at construction, not serialization.
	badMsg := &pingv1.PingResponse{Text: "\xc3\x28"} // invalid UTF-8
	_, err := connectproto.NewErrorDetail(badMsg)
	assert.NotNil(t, err)

	// Debug is best effort: an invalid Debug never fails serialization; it is
	// regenerated (when the type is registered) or omitted.
	detail, err := connectproto.NewErrorDetail(&pingv1.PingResponse{Number: 42})
	assert.Nil(t, err)
	detail.Debug = []byte("{invalid")
	data, err := json.Marshal((*connectWireDetail)(detail))
	assert.Nil(t, err)
	var reparsed connectWireDetail
	assert.Nil(t, json.Unmarshal(data, &reparsed))
	assert.Equal(t, reparsed.Type, detail.Type)
	assert.Equal(t, reparsed.Value, detail.Value)
}
