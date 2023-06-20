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

package connect

import (
	"net/http"

	"github.com/rs/cors"
)

type corsHandler struct {
	cors *cors.Cors
}

func newCORSHandler(config CORS) *corsHandler {
	options := cors.Options{
		AllowedMethods: []string{
			http.MethodGet,
			http.MethodPost,
		},
		AllowedHeaders: []string{
			"Content-Type",
			"Connect-Protocol-Version",
			"Connect-Timeout-Ms",
			"Connect-Accept-Encoding",  // future use
			"Connect-Content-Encoding", // future use
			"Accept-Encoding",          // future use
			"Content-Encoding",         // future use
			"Grpc-Timeout",             // gRPC-web
			"X-Grpc-Web",               // gRPC-web
			"X-User-Agent",             // gRPC-web
		},
		ExposedHeaders: []string{
			"Content-Encoding",
			"Connect-Content-Encoding",
			"Grpc-Status",             // gRPC-web
			"Grpc-Message",            // gRPC-web
			"Grpc-Status-Details-Bin", // gRPC, gRPC-web
		},
	}

	options.AllowOriginFunc = config.AllowOriginFunc
	options.AllowedHeaders = append(options.AllowedHeaders, config.AllowedHeaders...)
	options.ExposedHeaders = append(options.ExposedHeaders, config.ExposedHeaders...)
	options.MaxAge = config.MaxAge
	options.AllowCredentials = config.AllowCredentials

	return &corsHandler{
		cors: cors.New(options),
	}
}

func (c *corsHandler) applyToHandler(config *handlerConfig) {
	config.CORSHandler = c
}

func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}

// handle handles CORS for the request, returning true if the request is done.
func (c *corsHandler) handle(w http.ResponseWriter, r *http.Request) (done bool) {
	c.cors.HandlerFunc(w, r)
	return isPreflight(r)
}
