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
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
	"github.com/coder/websocket"
)

func TestWebSocketParseMarker(t *testing.T) {
	t.Parallel()
	const text, binary = websocket.MessageText, websocket.MessageBinary
	tests := []struct {
		name        string
		messageType websocket.MessageType
		data        string
		codecName   string
		wantMarker  byte
		wantPayload string
		wantErr     string
	}{
		{name: "metadata", messageType: text, data: "M[]", codecName: connect.CodecNameProto, wantMarker: 'M', wantPayload: "[]"},
		{name: "end_stream", messageType: text, data: "S{}", codecName: connect.CodecNameProto, wantMarker: 'S', wantPayload: "{}"},
		{name: "client_end_stream", messageType: text, data: "C", codecName: connect.CodecNameProto, wantMarker: 'C'},
		{name: "binary_body", messageType: binary, data: "B\x08\xff", codecName: connect.CodecNameProto, wantMarker: 'B', wantPayload: "\x08\xff"},
		{name: "text_body", messageType: text, data: "B{}", codecName: connect.CodecNameJSON, wantMarker: 'B', wantPayload: "{}"},
		// Section 6.1: an empty B goes to the codec, under either codec.
		{name: "empty_binary_body", messageType: binary, data: "B", codecName: connect.CodecNameProto, wantMarker: 'B'},
		{name: "empty_text_body", messageType: text, data: "B", codecName: connect.CodecNameJSON, wantMarker: 'B'},
		{name: "empty_message", messageType: text, data: "", codecName: connect.CodecNameProto, wantErr: "empty message"},
		{name: "empty_binary_message", messageType: binary, data: "", codecName: connect.CodecNameProto, wantErr: "empty message"},
		{name: "unknown_marker", messageType: text, data: "X{}", codecName: connect.CodecNameProto, wantErr: "unknown marker 0x58"},
		{name: "lower_case_marker", messageType: text, data: "m[]", codecName: connect.CodecNameProto, wantErr: "unknown marker 0x6d"},
		{name: "body_in_text_frame", messageType: text, data: "B", codecName: connect.CodecNameProto, wantErr: "expected a binary frame"},
		{name: "body_in_binary_frame", messageType: binary, data: "B{}", codecName: connect.CodecNameJSON, wantErr: "expected a text frame"},
		{name: "metadata_in_binary_frame", messageType: binary, data: "M[]", codecName: connect.CodecNameProto, wantErr: "expected a text frame"},
		{name: "end_stream_in_binary_frame", messageType: binary, data: "S{}", codecName: connect.CodecNameProto, wantErr: "expected a text frame"},
		{name: "client_end_stream_in_binary_frame", messageType: binary, data: "C", codecName: connect.CodecNameProto, wantErr: "expected a text frame"},
		{name: "client_end_stream_with_payload", messageType: text, data: "C{}", codecName: connect.CodecNameProto, wantErr: "C message has a payload"},
		{name: "metadata_without_payload", messageType: text, data: "M", codecName: connect.CodecNameProto, wantErr: "M message has no payload"},
		{name: "end_stream_without_payload", messageType: text, data: "S", codecName: connect.CodecNameProto, wantErr: "S message has no payload"},
		{name: "invalid_utf8", messageType: text, data: "M\xff", codecName: connect.CodecNameProto, wantErr: "invalid UTF-8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			buffer := bytes.NewBufferString(test.data)
			marker, err := websocketParseMarker(test.messageType, buffer, test.codecName)
			if test.wantErr != "" {
				assert.NotNil(t, err)
				assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
				assert.True(t, strings.Contains(err.Message(), test.wantErr), assert.Sprintf("message %q", err.Message()))
				assert.True(t, errors.Is(err, errWebSocketProtocol) || errors.Is(err, errWebSocketInvalidUTF8))
				return
			}
			assert.Nil(t, err)
			assert.Equal(t, marker, test.wantMarker)
			assert.Equal(t, buffer.String(), test.wantPayload)
		})
	}
}

func TestWebSocketDecodeMetadata(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		payload    string
		fromClient bool
		want       http.Header
		wantErr    string
	}{
		{name: "empty", payload: "[]", want: http.Header{}},
		{name: "entries", payload: `[["x-a","1","2"],["x-b",""]]`, want: http.Header{"X-A": {"1", "2"}, "X-B": {""}}},
		{name: "trimmed", payload: `[["x-a"," \t1 2\t ","  "]]`, want: http.Header{"X-A": {"1 2", ""}}},
		{name: "bin_value_unchecked", payload: `[["x-a-bin","café"]]`, fromClient: true, want: http.Header{"X-A-Bin": {"café"}}},
		{name: "reserved_from_server", payload: `[["cookie","a"]]`, want: http.Header{"Cookie": {"a"}}},
		{name: "reserved_from_client", payload: `[["cookie","a"]]`, fromClient: true, wantErr: `key "cookie" is reserved`},
		{name: "reserved_prefix_from_client", payload: `[["x-forwarded-for","a"]]`, fromClient: true, wantErr: `key "x-forwarded-for" is reserved`},
		{name: "connect_prefix_from_client", payload: `[["connect-timeout-ms","1"]]`, fromClient: true, wantErr: `key "connect-timeout-ms" is reserved`},
		{name: "no_payload", payload: "", wantErr: "not an array of string arrays"},
		{name: "null", payload: "null", wantErr: "M message is null"},
		{name: "object", payload: `{"x-a":["1"]}`, wantErr: "not an array of string arrays"},
		{name: "entry_not_array", payload: `["x-a"]`, wantErr: "not an array of string arrays"},
		{name: "element_not_string", payload: `[["x-a",1]]`, wantErr: "not an array of string arrays"},
		{name: "null_element", payload: `[["x-a",null]]`, wantErr: "null element"},
		{name: "empty_entry", payload: `[[]]`, wantErr: "needs a key and at least one value"},
		{name: "key_only", payload: `[["x-a"]]`, wantErr: "needs a key and at least one value"},
		{name: "empty_key", payload: `[["","1"]]`, wantErr: "empty key"},
		{name: "invalid_key", payload: `[["x a","1"]]`, wantErr: "not a valid header name"},
		{name: "upper_case_key", payload: `[["X-A","1"]]`, wantErr: "not lower-case"},
		{name: "duplicate_key", payload: `[["x-a","1"],["x-a","2"]]`, wantErr: "more than once"},
		{name: "control_character", payload: `[["x-a","a\nb"]]`, wantErr: "not a valid header value"},
		{name: "delete_character", payload: `[["x-a","\u007f"]]`, wantErr: "not a valid header value"},
		{name: "obs_text", payload: `[["x-a","café"]]`, wantErr: "not a valid header value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := websocketDecodeMetadata([]byte(test.payload), test.fromClient)
			if test.wantErr != "" {
				assert.NotNil(t, err)
				assert.Equal(t, connect.CodeOf(err), connect.CodeInternal)
				assert.True(t, strings.Contains(err.Message(), test.wantErr), assert.Sprintf("message %q", err.Message()))
				assert.ErrorIs(t, err, errWebSocketProtocol)
				return
			}
			assert.Nil(t, err)
			assert.Equal(t, got, test.want)
		})
	}
}

func TestWebSocketParseTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		query   string
		want    time.Duration
		wantSet bool
		wantErr string
	}{
		{name: "absent", query: ""},
		{name: "other_parameter", query: "a=b"},
		{name: "zero", query: "connect-timeout-ms=0", wantSet: true},
		{name: "value", query: "connect-timeout-ms=50", want: 50 * time.Millisecond, wantSet: true},
		{name: "after_other_parameter", query: "a=b&connect-timeout-ms=50", want: 50 * time.Millisecond, wantSet: true},
		{name: "ten_digits", query: "connect-timeout-ms=9999999999", want: 9999999999 * time.Millisecond, wantSet: true},
		{name: "empty", query: "connect-timeout-ms=", wantErr: "no value supplied"},
		{name: "no_equals", query: "connect-timeout-ms", wantErr: "no value supplied"},
		{name: "eleven_digits", query: "connect-timeout-ms=12345678901", wantErr: "has >10 digits"},
		{name: "decimal", query: "connect-timeout-ms=1.5", wantErr: "has non-digit characters"},
		{name: "plus_sign", query: "connect-timeout-ms=%2B5", wantErr: "has non-digit characters"},
		{name: "minus_sign", query: "connect-timeout-ms=-1", wantErr: "has non-digit characters"},
		{name: "repeated", query: "connect-timeout-ms=1&connect-timeout-ms=1", wantErr: "more than one value"},
		{name: "malformed_escape", query: "connect-timeout-ms=1%zz", wantErr: "invalid URL escape"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, set, err := websocketParseTimeout(test.query)
			if test.wantErr != "" {
				assert.NotNil(t, err)
				assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument)
				assert.True(t, strings.Contains(err.Error(), test.wantErr), assert.Sprintf("error %q", err))
				return
			}
			assert.Nil(t, err)
			assert.Equal(t, got, test.want)
			assert.Equal(t, set, test.wantSet)
		})
	}
}
