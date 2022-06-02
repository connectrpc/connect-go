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

package main

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"

	"github.com/bufbuild/connect-go"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
	"github.com/bufbuild/connect-go/internal/gen/connect/ping/v1/pingv1connect"
	"golang.org/x/net/http2"
)

func newInsecureHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true, // Permits HTTP/2 requests using the insecure.
			DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) {
				return net.Dial(network, addr)
			},
		},
	}
}

func main() {
	// Create client.
	client := pingv1connect.NewPingServiceClient(
		newInsecureHTTPClient(),
		"https://localhost:8080/",
	)

	// Create request with type-safe payload and extra header.
	req := connect.NewRequest(&pingv1.PingRequest{Number: 42})
	req.Header().Set("Some-Header", "Hello from connect! 💪")

	// Call remote gRPC server.
	res, err := client.Ping(context.Background(), req)
	if err != nil {
		log.Fatalln(err)
	}

	// Enjoy the type-safe response!
	log.Println(res.Msg)
	log.Println("Response header:", res.Header().Get("Some-Other-Header"))
}
