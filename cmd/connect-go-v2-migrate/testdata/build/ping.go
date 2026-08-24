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

package app

import (
	"context"
	"errors"
	"io"
	"net/http"

	"connectrpc.com/connect"
	pingv1 "example.com/app/gen/connect/ping/v1"
	"example.com/app/gen/connect/ping/v1/pingv1connect"
)

type pingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (s *pingServer) Ping(ctx context.Context, req *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
	return connect.NewResponse(&pingv1.PingResponse{Number: req.Msg.Number, Text: req.Msg.Text}), nil
}

func setup() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(&pingServer{}))
	return mux
}
