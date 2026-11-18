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

package wstransport

import (
	"strings"

	"connectrpc.com/connect/v2"
)

// wsSubprotocolPrefix starts every token §4.3 defines: connectrpc.1+ and then
// the codec's name, as Connect over HTTP spells it in application/connect+name.
const wsSubprotocolPrefix = "connectrpc.1+"

// TextCodec is an optional interface for a [connect.Codec] whose encoding is
// UTF-8 text. Over WebSocket, the frame type tells a receiver how a body is
// encoded (§6.1), so a codec other than proto or json that returns true from
// IsText has its bodies sent in text frames; every other such codec's go in
// binary frames. The proto and json codecs are binary and text by definition,
// whatever they implement.
//
// A text codec must produce valid UTF-8, which RFC 6455 requires of a text
// frame. A body that is not is refused before it is sent.
type TextCodec interface {
	connect.Codec
	// IsText reports whether the codec's output is UTF-8 text.
	IsText() bool
}

// codecIsText reports whether codec's bodies travel in text frames.
func codecIsText(codec connect.Codec) bool {
	switch codec.Name() {
	case connect.CodecNameJSON:
		return true
	case connect.CodecNameProto:
		return false
	}
	textCodec, ok := codec.(TextCodec)
	return ok && textCodec.IsText()
}

// subprotocolForCodec is the token that names a codec.
func subprotocolForCodec(name string) string {
	return wsSubprotocolPrefix + name
}

// codecForSubprotocol maps a token to the name of the codec it selects, or ""
// when the token is not one §4.3 defines.
func codecForSubprotocol(token string) string {
	name, ok := strings.CutPrefix(token, wsSubprotocolPrefix)
	if !ok || !validCodecName(name) {
		return ""
	}
	return name
}

// validCodecName reports whether a token can carry name. §4.3 requires a
// lower-case HTTP token, which also keeps the subprotocol a valid one.
func validCodecName(name string) bool {
	if name == "" {
		return false
	}
	for index := range len(name) {
		if !isTokenByte(name[index]) || ('A' <= name[index] && name[index] <= 'Z') {
			return false
		}
	}
	return true
}

// carriedCodecs filters codecs to those a token can name, in the order given,
// keeping the first of any name registered twice.
func carriedCodecs(codecs []connect.Codec) []connect.Codec {
	seen := make(map[string]struct{}, len(codecs))
	carried := make([]connect.Codec, 0, len(codecs))
	for _, codec := range codecs {
		name := codec.Name()
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		if validCodecName(name) {
			carried = append(carried, codec)
		}
	}
	return carried
}
