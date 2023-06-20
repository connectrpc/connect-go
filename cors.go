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

// CORS config that adds Cross-Origin Resource Sharing (CORS) header support.
type CORS struct {
	// AllowOriginFunc is a custom function to validate the origin. It takes the
	// origin as argument and returns true if allowed or false otherwise.
	AllowOriginFunc func(origin string) bool

	// AllowedHeaders is list of non simple headers the client is allowed to
	// use with cross-domain requests.
	AllowedHeaders []string

	// ExposedHeaders indicates which headers are safe to expose to the API of
	// AllowHeaders.ExposedHeaders is ignored if the request's
	// Access-Control-Request-Headers header is empty.
	ExposedHeaders []string

	// MaxAge indicates how long (in seconds) the results of a preflight request
	// can be cached. Default value is 0 which means that the request is not
	// cached.
	MaxAge int

	// AllowCredentials indicates whether the request can include user credentials like
	// cookies, HTTP authentication or client side SSL certificates.
	AllowCredentials bool
}

func (c CORS) applyToHandler(config *handlerConfig) {
	config.CORS = &c
}

// wrap creates a corsHandler that wraps the given handlers.
// Methods are extracted from the handlers.
// If c is nil, nil is returned.
func (c *CORS) wrap(handlers []protocolHandler) *corsHandler {
	if c == nil {
		return nil
	}
	methods := make(map[string]struct{})
	for _, handler := range handlers {
		for method := range handler.Methods() {
			methods[method] = struct{}{}
		}
	}
	allow := make([]string, 0, len(methods))
	for ct := range methods {
		allow = append(allow, ct)
	}

	options := cors.Options{
		AllowedMethods: allow,
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

	options.AllowOriginFunc = c.AllowOriginFunc
	options.AllowedHeaders = append(options.AllowedHeaders, c.AllowedHeaders...)
	options.ExposedHeaders = append(options.ExposedHeaders, c.ExposedHeaders...)
	options.MaxAge = c.MaxAge
	options.AllowCredentials = c.AllowCredentials
	return &corsHandler{
		cors: cors.New(options),
	}
}

type corsHandler struct {
	cors *cors.Cors
}

func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}

// handle handles CORS for the request, returning true if the request is completed.
func (c *corsHandler) handle(w http.ResponseWriter, r *http.Request) bool {
	c.cors.HandlerFunc(w, r)
	return isPreflight(r)
}
