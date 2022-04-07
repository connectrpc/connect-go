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
	"log"
)

// defaultWarn is the default function used to log warnings. Users can replace
// it with WithWarn.
func defaultWarn(err error) {
	log.Printf("warn: %v", err) // nolint:forbidigo
}

// newWarnIfError wraps a log function to automatically ignore nil errors. This
// cleans up the code handling errors quite a bit - it doesn't need to name and
// nil-check tons of errors.
func newWarnIfError(warn func(error)) func(error) {
	return func(err error) {
		if err != nil {
			warn(err)
		}
	}
}
