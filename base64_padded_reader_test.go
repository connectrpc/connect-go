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
	"bytes"
	"encoding/base64"
	"io"
	"testing"
	"testing/quick"
)

func TestPaddedReader(t *testing.T) {
	t.Parallel()
	makeRoundtrip := func(padding rune) func([]byte) bool {
		return func(data []byte) bool {
			encoding := base64.URLEncoding.WithPadding(padding)
			encodedData := make([]byte, encoding.EncodedLen(len(data)))
			encoding.Encode(encodedData, data)

			reader := newBase64PaddedReader(bytes.NewBuffer(encodedData))
			decoder := base64.NewDecoder(base64.URLEncoding, reader)
			decodedData, err := io.ReadAll(decoder)
			if err != nil {
				t.Fatal(err)
			}

			return bytes.Equal(decodedData, data)
		}
	}

	if err := quick.Check(makeRoundtrip(base64.NoPadding), nil); err != nil {
		t.Error(err)
	}

	if err := quick.Check(makeRoundtrip(base64.StdPadding), nil); err != nil {
		t.Error(err)
	}
}
