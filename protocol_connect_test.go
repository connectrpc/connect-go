// Copyright 2021-2024 The Connect Authors
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
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/internal/assert"
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
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			detail, err := NewErrorDetail(testCase.errorDetail)
			assert.Nil(t, err)
			data, err := json.Marshal((*connectWireDetail)(detail))
			assert.Nil(t, err)
			t.Logf("marshaled error detail: %s", string(data))

			var unmarshaled connectWireDetail
			assert.Nil(t, json.Unmarshal(data, &unmarshaled))
			assert.Equal(t, unmarshaled.wireJSON, string(data))
			assert.Equal(t, unmarshaled.pb, detail.pb)

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
	raw := `{"type":"acme.user.v1.User","value":"DEADBF",` +
		`"debug":{"email":"someone@connectrpc.com"}}`
	var detail connectWireDetail
	assert.Nil(t, json.Unmarshal([]byte(raw), &detail))
	assert.Equal(t, detail.pb.GetTypeUrl(), defaultAnyResolverPrefix+"acme.user.v1.User")

	_, err := (*ErrorDetail)(&detail).Value()
	assert.NotNil(t, err)
	assert.True(t, strings.HasSuffix(err.Error(), "not found"))

	encoded, err := json.Marshal(&detail)
	assert.Nil(t, err)
	assert.Equal(t, string(encoded), raw)
}

func TestConnectEndOfResponseCanonicalTrailers(t *testing.T) {
	t.Parallel()

	buffer := bytes.Buffer{}
	bufferPool := newBufferPool()

	endStreamMessage := connectEndStreamMessage{Trailer: make(http.Header)}
	endStreamMessage.Trailer["not-canonical-header"] = []string{"a"}
	endStreamMessage.Trailer["mixed-Canonical"] = []string{"b"}
	endStreamMessage.Trailer["Mixed-Canonical"] = []string{"b"}
	endStreamMessage.Trailer["Canonical-Header"] = []string{"c"}
	endStreamData, err := json.Marshal(endStreamMessage)
	assert.Nil(t, err)

	writer := envelopeWriter{
		sender:     writeSender{writer: &buffer},
		bufferPool: bufferPool,
	}
	err = writer.Write(&envelope{
		Flags: connectFlagEnvelopeEndStream,
		Data:  bytes.NewBuffer(endStreamData),
	})
	assert.Nil(t, err)

	unmarshaler := connectStreamingUnmarshaler{
		envelopeReader: envelopeReader{
			reader:     &buffer,
			bufferPool: bufferPool,
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
		expectCode           Code
		expectBadContentType bool
		expectNotModified    bool
	}{
		// Allowed content-types for OK responses.
		{
			codecName:           codecNameProto,
			statusCode:          http.StatusOK,
			responseContentType: "application/proto",
		},
		{
			codecName:           codecNameJSON,
			statusCode:          http.StatusOK,
			responseContentType: "application/json",
		},
		{
			codecName:           codecNameJSON,
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
			codecName:           codecNameProto,
			statusCode:          http.StatusNotFound,
			responseContentType: "application/json",
		},
		{
			codecName:           codecNameProto,
			statusCode:          http.StatusBadRequest,
			responseContentType: "application/json; charset=utf-8",
		},
		{
			codecName:           codecNameJSON,
			statusCode:          http.StatusInternalServerError,
			responseContentType: "application/json",
		},
		{
			codecName:           codecNameJSON,
			statusCode:          http.StatusPreconditionFailed,
			responseContentType: "application/json; charset=utf-8",
		},
		// 304 Not Modified for GET request gets a special error, regardless of content-type
		{
			codecName:           codecNameProto,
			get:                 true,
			statusCode:          http.StatusNotModified,
			responseContentType: "application/json",
			expectCode:          CodeUnknown,
			expectNotModified:   true,
		},
		{
			codecName:           codecNameJSON,
			get:                 true,
			statusCode:          http.StatusNotModified,
			responseContentType: "application/json",
			expectCode:          CodeUnknown,
			expectNotModified:   true,
		},
		// OK status, invalid content-type
		{
			codecName:            codecNameProto,
			statusCode:           http.StatusOK,
			responseContentType:  "application/proto; charset=utf-8",
			expectCode:           CodeInternal,
			expectBadContentType: true,
		},
		{
			codecName:            codecNameProto,
			statusCode:           http.StatusOK,
			responseContentType:  "application/json",
			expectCode:           CodeInternal,
			expectBadContentType: true,
		},
		{
			codecName:            codecNameJSON,
			statusCode:           http.StatusOK,
			responseContentType:  "application/proto",
			expectCode:           CodeInternal,
			expectBadContentType: true,
		},
		{
			codecName:            codecNameJSON,
			statusCode:           http.StatusOK,
			responseContentType:  "some/garbage",
			expectCode:           CodeInternal,
			expectBadContentType: true,
		},
		// Error status, invalid content-type, returns code based on HTTP status code
		{
			codecName:           codecNameProto,
			statusCode:          http.StatusNotFound,
			responseContentType: "application/proto",
			expectCode:          connectHTTPToCode(http.StatusNotFound),
		},
		{
			codecName:           codecNameJSON,
			statusCode:          http.StatusBadRequest,
			responseContentType: "some/garbage",
			expectCode:          connectHTTPToCode(http.StatusBadRequest),
		},
		{
			codecName:           codecNameJSON,
			statusCode:          http.StatusTooManyRequests,
			responseContentType: "some/garbage",
			expectCode:          connectHTTPToCode(http.StatusTooManyRequests),
		},
	}
	for _, testCase := range testCases {
		testCase := testCase
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
				assert.Equal(t, CodeOf(err), testCase.expectCode)
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
		expectErr           bool
	}{
		// Allowed content-types
		{
			codecName:           codecNameProto,
			responseContentType: "application/connect+proto",
		},
		{
			codecName:           codecNameJSON,
			responseContentType: "application/connect+json",
		},
		// Disallowed content-types
		{
			codecName:           codecNameProto,
			responseContentType: "application/proto",
			expectErr:           true,
		},
		{
			codecName:           codecNameJSON,
			responseContentType: "application/json",
			expectErr:           true,
		},
		{
			codecName:           codecNameJSON,
			responseContentType: "application/json; charset=utf-8",
			expectErr:           true,
		},
		{
			codecName:           codecNameJSON,
			responseContentType: "application/connect+json; charset=utf-8",
			expectErr:           true,
		},
		{
			codecName:           codecNameProto,
			responseContentType: "some/garbage",
			expectErr:           true,
		},
	}
	for _, testCase := range testCases {
		testCase := testCase
		testCaseName := fmt.Sprintf("%s->%s", testCase.codecName, testCase.responseContentType)
		t.Run(testCaseName, func(t *testing.T) {
			t.Parallel()
			err := connectValidateStreamResponseContentType(
				testCase.codecName,
				StreamTypeServer,
				testCase.responseContentType,
			)
			if !testCase.expectErr {
				assert.Nil(t, err)
			} else if assert.NotNil(t, err) {
				assert.Equal(t, CodeOf(err), CodeInternal)
				assert.True(t, strings.Contains(err.Message(), fmt.Sprintf("invalid content-type: %q; expecting", testCase.responseContentType)))
			}
		})
	}
}
