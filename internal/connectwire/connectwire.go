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

// Package connectwire implements the Connect protocol's JSON error
// representation and its end-of-stream envelope.
//
// Both are protocol concepts rather than HTTP ones, so transports other than
// net/http reuse them to terminate a stream the way Connect clients expect.
package connectwire

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/connecterr"
)

// FlagEnvelopeEndStream marks the envelope that ends a Connect stream. Its
// payload is an [EndStreamMessage].
const FlagEnvelopeEndStream = 0b00000010

// WireDetail adapts a [connect.ErrorDetail] to the Connect protocol's error
// detail object.
type WireDetail connect.ErrorDetail

// MarshalJSON implements [json.Marshaler].
func (d *WireDetail) MarshalJSON() ([]byte, error) {
	wire := struct {
		Type  string          `json:"type"`
		Value string          `json:"value"`
		Debug json.RawMessage `json:"debug,omitempty"`
	}{
		Type:  d.Type,
		Value: base64.RawStdEncoding.EncodeToString(d.Value),
	}
	if json.Valid(d.Debug) {
		wire.Debug = json.RawMessage(d.Debug)
	} else if msg, err := connectproto.ErrorDetailToAny((*connect.ErrorDetail)(d)).UnmarshalNew(); err == nil {
		var buffer bytes.Buffer
		var codec connectproto.JSONCodec
		if err := codec.MarshalWrite(context.Background(), &buffer, msg); err == nil {
			wire.Debug = buffer.Bytes()
		}
	}
	return json.Marshal(wire)
}

// UnmarshalJSON implements [json.Unmarshaler].
func (d *WireDetail) UnmarshalJSON(data []byte) error {
	var wire struct {
		Type  string          `json:"type"`
		Value string          `json:"value"`
		Debug json.RawMessage `json:"debug,omitempty"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	value, err := connect.DecodeBinaryHeader(wire.Value)
	if err != nil {
		return fmt.Errorf("decode base64: %w", err)
	}
	*d = WireDetail{
		Type:  wire.Type,
		Value: value,
		Debug: wire.Debug,
	}
	return nil
}

// WireError is the Connect protocol's JSON error object.
type WireError struct {
	Code    connect.Code  `json:"code"`
	Message string        `json:"message,omitempty"`
	Details []*WireDetail `json:"details,omitempty"`
}

// NewWireError converts err into its wire representation, defaulting to
// [connect.CodeUnknown] for errors that carry no code.
func NewWireError(err error) *WireError {
	wire := &WireError{
		Code:    connect.CodeUnknown,
		Message: err.Error(),
	}
	if connectErr, ok := connecterr.AsError(err); ok {
		wire.Code = connectErr.Code()
		wire.Message = connectErr.Message()
		if details := connectErr.Details(); len(details) > 0 {
			wire.Details = make([]*WireDetail, len(details))
			for i, detail := range details {
				wire.Details[i] = (*WireDetail)(detail)
			}
		}
	}
	return wire
}

// AsError converts the wire representation back into a [connect.Error] marked
// as a peer's verdict.
func (e *WireError) AsError() *connect.Error {
	if e == nil {
		return nil
	}
	if e.Code < connect.CodeCanceled || e.Code > connect.CodeUnauthenticated {
		e.Code = connect.CodeUnknown
	}
	err := connect.NewError(e.Code, e.Message).WithRemote()
	if len(e.Details) > 0 {
		for _, detail := range e.Details {
			err = err.WithDetail((*connect.ErrorDetail)(detail))
		}
	}
	return err
}

// UnmarshalJSON implements [json.Unmarshaler].
func (e *WireError) UnmarshalJSON(data []byte) error {
	// We want to be lenient if the JSON has an unrecognized or invalid code.
	// So if that occurs, we leave the code unset but can still de-serialize
	// the other fields from the input JSON.
	var wireError struct {
		Code    string        `json:"code"`
		Message string        `json:"message"`
		Details []*WireDetail `json:"details"`
	}
	err := json.Unmarshal(data, &wireError)
	if err != nil {
		return err
	}
	e.Message = wireError.Message
	e.Details = wireError.Details
	// This will leave e.Code unset if we can't unmarshal the given string.
	_ = e.Code.UnmarshalText([]byte(wireError.Code))
	return nil
}

// EndStreamMessage is the JSON payload of the end-of-stream envelope.
type EndStreamMessage struct {
	Error   *WireError  `json:"error,omitempty"`
	Trailer http.Header `json:"metadata,omitempty"`
}
