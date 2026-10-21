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

package connect

import (
	"errors"
	"net/http"

	"connectrpc.com/connect/v2/connecthttp"
)

// An ErrorWriter writes errors to an [http.ResponseWriter] in the format
// expected by an RPC client. This is especially useful in server-side net/http
// middleware, where you may wish to handle requests from RPC and non-RPC
// clients with the same code.
//
// ErrorWriters are safe to use concurrently.
type ErrorWriter struct {
	inner *connecthttp.ErrorWriter
}

// NewErrorWriter constructs an ErrorWriter. Handler options may be passed to
// configure the error writer behaviour to match the handlers.
// [WithRequireConnectProtocolHeader] will assert that Connect protocol
// requests include the version header allowing the error writer to correctly
// classify the request.
//
// Options supplied via [WithConditionalHandlerOptions] are ignored.
func NewErrorWriter(opts ...HandlerOption) *ErrorWriter {
	cfg := newHandlerConfig("", StreamTypeUnary, opts)
	mountOpts := handlerMountOptions(cfg)
	return &ErrorWriter{inner: connecthttp.NewErrorWriter(mountOpts...)}
}

// IsSupported checks whether a request is using one of the ErrorWriter's
// supported RPC protocols.
func (w *ErrorWriter) IsSupported(request *http.Request) bool {
	return w.inner.IsSupported(request)
}

// Write an error, using the format appropriate for the RPC protocol in use.
// Callers should first use IsSupported to verify that the request is using one
// of the ErrorWriter's supported RPC protocols. If the protocol is unknown,
// Write will send the error as unprefixed, Connect-formatted JSON.
//
// Write does not read or close the request body.
func (w *ErrorWriter) Write(response http.ResponseWriter, request *http.Request, err error) error {
	// NewNotModifiedError signals a Connect-GET conditional response. v2's
	// connecthttp.ErrorWriter has no 304 path, so emit it here: write the
	// user's headers (Etag, Cache-Control, ...) and a bare 304, matching v1.
	// Only Connect GET requests qualify; gRPC and gRPC-Web are POST-only and
	// treat the sentinel as a CodeUnknown error, so they fall through.
	if request.Method == http.MethodGet && errors.Is(err, errNotModified) {
		if v1err, ok := asError(err); ok {
			writeNonProtocolHeaders(response.Header(), v1err.meta)
		}
		response.WriteHeader(http.StatusNotModified)
		return nil
	}
	v2err := errToConnectv2(err)
	v1err, _ := asError(err)
	if v1err != nil && !v1err.wireErr && len(v1err.meta) > 0 {
		writeNonProtocolHeaders(response.Header(), v1err.meta)
	}
	return w.inner.Write(response, request, v2err)
}

// writeNonProtocolHeaders copies user-controlled headers from meta onto
// dst, skipping HTTP- and protocol-reserved keys. Used by ErrorWriter to
// surface a v1 [*Error.Meta] on the wire alongside the v2-encoded body.
func writeNonProtocolHeaders(dst, meta http.Header) {
	for key, vals := range meta {
		if len(vals) == 0 || isProtocolReservedHeader(key) {
			continue
		}
		dst[http.CanonicalHeaderKey(key)] = append([]string(nil), vals...)
	}
}

// errToConnectv2 converts an arbitrary error to a v2 *connectv2.Error so
// connecthttp serializers can write it. v1 *Error values are translated; all
// other errors are returned as-is and connecthttp treats them as
// CodeUnknown.
func errToConnectv2(err error) error {
	if err == nil {
		return nil
	}
	v1err, ok := asError(err)
	if !ok {
		return err
	}
	return v1err.toConnectv2()
}
