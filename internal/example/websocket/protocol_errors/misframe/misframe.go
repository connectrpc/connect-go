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

// Package misframe holds a server that answers with deliberately malformed
// frames, so the protocol_errors example can demonstrate the client-side
// protocol error handler.
//
// It is its own package because both halves of the example need it: the server
// binary serves it under -misframe, and the client's test points at it. Two
// package mains cannot share code, and a second copy could drift from the one
// the example actually runs.
package misframe

import (
	"net/http"

	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

// Handler answers an upgrade with a response carrying a marker nobody has
// defined — the mistake a client reports as connectwebsocket.FaultMarker.
//
// It speaks the wire format by hand rather than going through
// connectwebsocket, because that transport frames correctly and no option
// makes it stop.
func Handler(responseWriter http.ResponseWriter, request *http.Request) {
	conn, err := websocket.Accept(responseWriter, request, &websocket.AcceptOptions{
		Subprotocols:       []string{"connectrpc.1+proto"},
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	ctx := request.Context()
	if _, _, err := conn.Read(ctx); err != nil { // the client's request
		return
	}
	payload, err := proto.Marshal(&v1.CumSumResponse{Sum: 7})
	if err != nil {
		return
	}
	frame := append([]byte("Z"), payload...) // the lie
	_ = conn.Write(ctx, websocket.MessageBinary, frame)
}
