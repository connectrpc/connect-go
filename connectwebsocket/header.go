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

package connectwebsocket

import "net/http"

// mergeHeaders appends every value in from onto into.
//
// Keys carrying no values are skipped: net/http pre-populates trailer keys
// from the Trailer header, and those placeholders must not become entries.
func mergeHeaders(into, from http.Header) {
	for key, values := range from {
		for _, value := range values {
			into.Add(key, value)
		}
	}
}
