// Copyright 2021-2023 Buf Technologies, Inc.
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

package grpcadapt

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	connect "github.com/bufbuild/connect-go"
	statusv1 "github.com/bufbuild/connect-go/internal/gen/connectext/grpc/status/v1"
	"google.golang.org/protobuf/proto"
)

var (
	errTrailersWithoutGRPCStatus = fmt.Errorf("gRPC protocol error: no %s trailer", grpcHeaderStatus)
)

func getHeaderCanonical(header http.Header, key string) string {
	if v := header[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}
func setHeaderCanonical(h http.Header, key, value string) {
	h[key] = []string{value}
}
func delHeaderCanonical(h http.Header, key string) {
	delete(h, key)
}

func binaryQueryValueReader(data string) io.Reader {
	stringReader := strings.NewReader(data)
	if len(data)%4 != 0 {
		// Data definitely isn't padded.
		return base64.NewDecoder(base64.RawURLEncoding, stringReader)
	}
	// Data is padded, or no padding was necessary.
	return base64.NewDecoder(base64.URLEncoding, stringReader)
}

func queryValueReader(data string, base64Encoded bool) io.Reader {
	if base64Encoded {
		return binaryQueryValueReader(data)
	}
	return strings.NewReader(data)
}

func grpcPercentDecode(encoded string) string {
	for i := 0; i < len(encoded); i++ {
		if c := encoded[i]; c == '%' && i+2 < len(encoded) {
			return grpcPercentDecodeSlow(encoded, i)
		}
	}
	return encoded
}

// Similar to percentEncodeSlow: encoded is percent-encoded, and needs to be
// decoded byte-by-byte starting at offset.
func grpcPercentDecodeSlow(encoded string, offset int) string {
	var out strings.Builder
	out.Grow(len(encoded))
	out.WriteString(encoded[:offset])
	for i := offset; i < len(encoded); i++ {
		c := encoded[i]
		if c != '%' || i+2 >= len(encoded) {
			out.WriteByte(c)
			continue
		}
		parsed, err := strconv.ParseUint(encoded[i+1:i+3], 16 /* hex */, 8 /* bitsize */)
		if err != nil {
			out.WriteRune(utf8.RuneError)
		} else {
			out.WriteByte(byte(parsed))
		}
		i += 2
	}
	return out.String()
}

// The gRPC wire protocol specifies that errors should be serialized using the
// binary Protobuf format, even if the messages in the request/response stream
// use a different codec. Consequently, this function needs a Protobuf codec to
// unmarshal error information in the headers.
func grpcErrorFromTrailer(trailer http.Header) *connect.Error {
	codeHeader := trailer.Get(grpcHeaderStatus)
	if codeHeader == "" {
		return connect.NewError(connect.CodeInternal, errTrailersWithoutGRPCStatus)
	}
	if codeHeader == "0" {
		return nil
	}

	code, err := strconv.ParseUint(codeHeader, 10 /* base */, 32 /* bitsize */)
	if err != nil {
		err := fmt.Errorf("gRPC protocol error: invalid error code %q: %w", codeHeader, err)
		return connect.NewError(connect.CodeInternal, err)
	}
	if code == 0 {
		return nil
	}

	detailsBinaryEncoded := getHeaderCanonical(trailer, grpcHeaderDetails)
	if len(detailsBinaryEncoded) == 0 {
		message := grpcPercentDecode(getHeaderCanonical(trailer, grpcHeaderMessage))
		return connect.NewWireError(connect.Code(code), errors.New(message))
	}

	// Prefer the Protobuf-encoded data to the headers (grpc-go does this too).
	detailsBinary, err := connect.DecodeBinaryHeader(detailsBinaryEncoded)
	if err != nil {
		err := fmt.Errorf("server returned invalid grpc-status-details-bin trailer: %w", err)
		return connect.NewError(connect.CodeInternal, err)
	}
	var status statusv1.Status
	if err := proto.Unmarshal(detailsBinary, &status); err != nil {
		err := fmt.Errorf("server returned invalid protobuf for error details: %w", err)
		return connect.NewWireError(connect.CodeInternal, err)
	}
	trailerErr := connect.NewWireError(connect.Code(status.Code), errors.New(status.Message))
	for _, msg := range status.Details {
		errDetail, _ := connect.NewErrorDetail(msg)
		trailerErr.AddDetail(errDetail)
	}
	return trailerErr
}

func getGRPCTrailer(header http.Header) (http.Header, *connect.Error) {
	isTrailer := map[string]bool{
		// Force GRPC status values, try copy them directly from header.
		grpcHeaderStatus:  true,
		grpcHeaderDetails: true,
		grpcHeaderMessage: true,
	}
	for _, key := range strings.Split(header.Get(headerTrailer), ",") {
		key = http.CanonicalHeaderKey(key)
		isTrailer[key] = true
	}

	trailer := make(http.Header)
	for key, vals := range header {
		if strings.HasPrefix(key, http.TrailerPrefix) {
			// Must remove trailer prefix before canonicalizing.
			key = http.CanonicalHeaderKey(key[len(http.TrailerPrefix):])
			trailer[key] = vals
		} else if key := http.CanonicalHeaderKey(key); isTrailer[key] {
			trailer[key] = vals
		}
	}

	trailerErr := grpcErrorFromTrailer(trailer)
	if trailerErr != nil {
		return trailer, trailerErr
	}

	// Remove all protocol trailer keys.
	for key := range trailer {
		if isProtocolHeader(key) {
			delete(trailer, key)
		}
	}
	return trailer, nil
}
