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

package connect

import (
	"encoding/json"
	"net/http"
)

// WriteError should be called from HTTP middlewares to handle connect errors. It
// writes a status code encodes a connect error as JSON,  and writes to the response.
func WriteError(responseWriter http.ResponseWriter, err error) error {
	responseWriter.Header().Set(headerContentType, connectUnaryContentTypeJSON)
	responseWriter.WriteHeader(connectCodeToHTTP(CodeOf(err)))
	data, marshalErr := json.Marshal(newConnectWireError(err))
	if marshalErr != nil {
		return errorf(CodeInternal, "marshal error: %w", err)
	}
	_, writeErr := responseWriter.Write(data)
	if writeErr != nil {
		return writeErr
	}
	return nil
}
