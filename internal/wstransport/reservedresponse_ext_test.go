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

package wstransport_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"google.golang.org/protobuf/proto"
)

// connectKeyServer sets a connect- key alongside an ordinary one in both its
// headers and its trailers.
type connectKeyServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (connectKeyServer) Ping(ctx context.Context, request *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		info.ResponseHeader().Set("acme-header", "kept")
		info.ResponseHeader().Set("connect-sneaky-header", "dropped")
		info.ResponseTrailer().Set("acme-trailer", "kept")
		info.ResponseTrailer().Set("connect-sneaky-trailer", "dropped")
	}
	return &pingv1.PingResponse{Number: request.Number}, nil
}

// §7.3.2: connect- names belong to the protocol, so a server leaves any its
// handler set out of M and out of the trailers in S.
func TestServerOmitsConnectKeysFromResponseMetadata(t *testing.T) {
	t.Parallel()
	httpServer := newServerFor(t, connectKeyServer{})
	client := dialBrowserClient(t, httpServer, pingv1connect.PingServicePingProcedure)
	payload, err := proto.Marshal(&pingv1.PingRequest{Number: 7})
	assert.Nil(t, err)
	client.writeProto(t, payload)
	client.writeJSON(t, wireClientEndStream, nil)

	marker, metadata, _ := client.readMessage(t)
	assert.Equal(t, marker, wireMetadata)
	assert.True(t, strings.Contains(string(metadata), "acme-header"))
	assert.False(t, strings.Contains(string(metadata), "connect-"))

	marker, _, _ = client.readMessage(t)
	assert.Equal(t, marker, wireBody)

	marker, endStream, _ := client.readMessage(t)
	assert.Equal(t, marker, wireServerEndStream)
	assert.True(t, strings.Contains(string(endStream), "acme-trailer"))
	assert.False(t, strings.Contains(string(endStream), "connect-"))
}
