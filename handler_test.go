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

package connect_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bufbuild/connect"
	"github.com/bufbuild/connect/internal/assert"
	pingrpc "github.com/bufbuild/connect/internal/gen/proto/connect/connect/ping/v1test"
	pingpb "github.com/bufbuild/connect/internal/gen/proto/go/connect/ping/v1test"
	"google.golang.org/protobuf/proto"
)

func TestHandlerReadMaxBytes(t *testing.T) {
	const readMaxBytes = 32
	mux := http.NewServeMux()
	mux.Handle(pingrpc.NewPingServiceHandler(
		&ExamplePingServer{},
		connect.WithReadMaxBytes(readMaxBytes),
	))

	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := pingrpc.NewPingServiceClient(
		server.Client(),
		server.URL,
		connect.WithGRPC(),
	)
	assert.Nil(t, err)

	padding := "padding                      "
	req := &pingpb.PingRequest{Number: 42, Text: padding}
	// Ensure that the probe is actually too big.
	probeBytes, err := proto.Marshal(req)
	assert.Nil(t, err)
	assert.Equal(t, len(probeBytes), readMaxBytes+1)

	_, err = client.Ping(context.Background(), connect.NewEnvelope(req))

	assert.NotNil(t, err)
	assert.Equal(t, connect.CodeOf(err), connect.CodeInvalidArgument)
	const expect = "larger than configured max"
	assert.True(
		t,
		strings.Contains(err.Error(), expect),
		assert.Sprintf("error msg %q contains %q", err.Error(), expect),
	)
}
