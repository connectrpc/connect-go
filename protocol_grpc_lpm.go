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
	"bufio"
	"errors"
	"net/http"
	"net/textproto"
)

const flagLPMTrailer = 0b10000000

type grpcMarshaler struct {
	envelopeWriter
}

func (m *grpcMarshaler) MarshalWebTrailers(trailer http.Header) *Error {
	raw := m.envelopeWriter.bufferPool.Get()
	defer m.envelopeWriter.bufferPool.Put(raw)
	if err := trailer.Write(raw); err != nil {
		return errorf(CodeInternal, "format trailers: %w", err)
	}
	return m.Write(&envelope{
		Data:  raw,
		Flags: flagLPMTrailer,
	})
}

type grpcUnmarshaler struct {
	envelopeReader envelopeReader
	web            bool
	webTrailer     http.Header
}

func (u *grpcUnmarshaler) Unmarshal(message any) *Error {
	err := u.envelopeReader.Unmarshal(message)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errSpecialEnvelope) {
		return err
	}
	env := u.envelopeReader.last
	if !u.web || !env.IsSet(flagLPMTrailer) {
		return errorf(CodeUnknown, "protocol error: invalid envelope flags %d", env.Flags)
	}

	// Per the gRPC-Web specification, trailers should be encoded as an HTTP/1
	// headers block _without_ the terminating newline. To make the headers
	// parseable by net/textproto, we need to add the newline.
	if err := env.Data.WriteByte('\n'); err != nil {
		return errorf(CodeUnknown, "unmarshal web trailers: %w", err)
	}
	bufferedReader := bufio.NewReader(env.Data)
	mimeReader := textproto.NewReader(bufferedReader)
	mimeHeader, mimeErr := mimeReader.ReadMIMEHeader()
	if mimeErr != nil {
		return errorf(
			CodeInvalidArgument,
			"gRPC-Web protocol error: received invalid trailers: %w",
			mimeErr,
		)
	}
	u.webTrailer = http.Header(mimeHeader)
	return errSpecialEnvelope
}

func (u *grpcUnmarshaler) WebTrailer() http.Header {
	return u.webTrailer
}
