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
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"connectrpc.com/connect/v2"
	"github.com/coder/websocket"
)

// frameDecoder is the inbound framing both ends share: it validates a frame,
// decodes bodies and leading metadata, and classifies each fault the peer's
// framing causes. What a message means in sequence stays with each end.
type frameDecoder struct {
	// fromServer is set on a client, whose peer is the server. It names the
	// peer in errors and decides who a body the codec rejects blames.
	fromServer   bool
	codec        connect.Codec
	readMaxBytes int
	// bodyIsText is the frame type the negotiated codec uses. A body in the
	// other one is a protocol error.
	bodyIsText bool
	// fault classifies the most recent peer fault for the protocol error
	// monitor.
	fault ProtocolFault
}

func (d *frameDecoder) peer() string {
	if d.fromServer {
		return "server"
	}
	return "client"
}

// fail records what kind of fault this is before building its error, so a
// monitor can tell them apart without matching message strings.
//
// §5 reports a protocol error as Internal. A request body the codec cannot
// decode is not one — the framing was fine — so it keeps Connect-over-HTTP's
// InvalidArgument. On a client that code would blame the caller's request, so
// a malformed response stays Internal, as in Connect over HTTP.
func (d *frameDecoder) fail(fault ProtocolFault, format string, args ...any) *connect.Error {
	d.fault = fault
	if fault == FaultMessageEncoding && !d.fromServer {
		return errorf(connect.CodeInvalidArgument, format, args...)
	}
	return errorf(connect.CodeInternal, format, args...)
}

// parseFrame validates a frame and returns the message it carries. On invalid
// UTF-8 it sets fault to FaultTextEncoding, which ends the read path for good.
func (d *frameDecoder) parseFrame(messageType websocket.MessageType, frame []byte) (wireMessage, *connect.Error) {
	text := messageType == websocket.MessageText
	if !text && messageType != websocket.MessageBinary {
		return wireMessage{}, d.fail(FaultFrameType, "unknown WebSocket message type %d", messageType)
	}
	// §6.1: validated before the marker is read, so that invalid UTF-8 is
	// never mistaken for an unknown marker.
	if utf8Err := textEncodingError(frame, text); utf8Err != nil {
		d.fault = FaultTextEncoding
		return wireMessage{}, utf8Err
	}
	wire, decodeErr := decodeMessage(frame, text)
	if decodeErr != nil {
		d.fault = FaultMarker
		return wireMessage{}, decodeErr
	}
	// §12: the limit covers the payload, not the marker.
	if d.readMaxBytes > 0 && len(wire.payload) > d.readMaxBytes {
		d.fault = FaultSizeLimit
		return wireMessage{}, errorf(
			connect.CodeResourceExhausted,
			"message size %d is larger than configured max %d", len(wire.payload), d.readMaxBytes,
		)
	}
	return wire, nil
}

// decodeBody decodes a B message's payload with the negotiated codec.
func (d *frameDecoder) decodeBody(ctx context.Context, wire wireMessage, message any) *connect.Error {
	if wire.text != d.bodyIsText {
		// The subprotocol named one encoding and the frame names another. This
		// end may well hold a codec for what arrived, which is exactly why it
		// has to be refused rather than decoded: accepting it would let a peer
		// choose an encoding the handshake did not agree on (§6.1).
		return d.fail(
			FaultFrameType,
			"%s sent a body in a %s frame; the negotiated %s codec expects a %s frame",
			d.peer(), frameName(wire.text), d.codec.Name(), frameName(d.bodyIsText),
		)
	}
	if err := d.codec.UnmarshalRead(ctx, bytes.NewReader(wire.payload), message); err != nil {
		return d.fail(FaultMessageEncoding, "unmarshal message: %w", err)
	}
	return nil
}

// metadataEntries parses an M message's payload into its entries, before
// any check on what they name.
func (d *frameDecoder) metadataEntries(payload []byte) ([][]string, *connect.Error) {
	// The empty array, never null: an M carries a JSON array, and a bare
	// marker is not one.
	if len(payload) == 0 {
		return nil, d.fail(FaultMetadata, "empty M message; empty metadata is []")
	}
	var entries [][]string
	if err := json.Unmarshal(payload, &entries); err != nil {
		return nil, d.fail(FaultMetadata, "unmarshal M message: %w", err)
	}
	// json.Unmarshal accepts null for a slice; [] decodes to an empty one.
	if entries == nil {
		return nil, d.fail(FaultMetadata, "M message is null; empty metadata is []")
	}
	return entries, nil
}

// metadataHeader turns M message entries into the headers they set.
func (d *frameDecoder) metadataHeader(entries [][]string) (http.Header, *connect.Error) {
	meta, err := decodeMetadata(entries)
	if err != nil {
		d.fault = FaultMetadata
		return nil, err
	}
	return meta, nil
}
