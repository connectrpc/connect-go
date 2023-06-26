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
)

// CORS config to help implement Cross-Origin Resource Sharing (CORS) header support.
type CORS struct {
	// AllowedMethods that scripts running in the browser are permitted to use.
	//
	// To support cross-domain requests with the protocols supported by Connect,
	// these headers fields must be included in the preflight response header
	// Access-Control-Allow-Methods.
	AllowedMethods []string

	// AllowedHeader fields that scripts running in the browser are permitted
	// to send.
	//
	// To support cross-domain requests with the protocols supported by Connect,
	// these field names must be included in the preflight response header
	// Access-Control-Allow-Headers.
	//
	// Make sure to include any application-specific headers your browser client
	// may send.
	AllowedHeaders []string

	// ExposedHeader fields that scripts running the browser are permitted
	// to see.
	//
	// To support cross-domain requests with the protocols supported by Connect,
	// these field names must be included in header Access-Control-Expose-Headers
	// of the actual response.
	//
	// Make sure to include any application-specific headers your browser client
	// should see. If your application uses trailers, they will be sent as header
	// fields with a `Trailer-` prefix for Connect unary RPCs. Ensure to
	// include them as well if you want them to be visible in all supported
	// protocols.
	ExposedHeaders []string
}

// CORSDefault returns a new CORS config object with the default required
// configuration for Connect and supported protocols.
//
// Make sure to include application-specific headers that your application
// uses as well in addition to these default headers.
func CORSDefault() CORS {
	return CORS{
		AllowedMethods: []string{http.MethodGet, http.MethodPost},
		AllowedHeaders: []string{
			headerContentType,
			connectHeaderProtocolVersion,
			connectHeaderTimeout,
			connectUnaryHeaderCompression,           // Unused in web browsers, but added for future-proofing
			connectUnaryHeaderAcceptCompression,     // Unused in web browsers, but added for future-proofing
			connectStreamingHeaderCompression,       // Unused in web browsers, but added for future-proofing
			connectStreamingHeaderAcceptCompression, // Unused in web browsers, but added for future-proofing
			grpcHeaderTimeout,                       // Used for gRPC and gRPC-web
			grpcHeaderMessageType,                   // Unused in web browsers, but added for future-proofing
			grpcWebHeaderXGrpcWeb,                   // Used for gRPC-web
			grpcWebHeaderXUserAgent,                 // Used for gRPC-web
		},
		ExposedHeaders: []string{
			connectUnaryHeaderCompression,     // Unused in web browsers, but added for future-proofing
			connectStreamingHeaderCompression, // Unused in web browsers, but added for future-proofing
			grpcHeaderStatus,                  // Required for gRPC-web
			grpcHeaderMessage,                 // Required for gRPC-web
			grpcHeaderDetails,                 // Error details in gRPC, gRPC-web
		},
	}
}
