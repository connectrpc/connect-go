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
	"encoding/base64"
	"io"
)

// base64PaddedReader wraps an underlying reader and ensures that the data read
// is properly terminated with base64 padding. This is useful because Go's
// base64 library does not support decoding while allowing either padded or
// unpadded input simultaneously.
type base64PaddedReader struct {
	reader  io.Reader
	counter int
}

func newBase64PaddedReader(reader io.Reader) io.Reader {
	return &base64PaddedReader{
		reader:  reader,
		counter: 0,
	}
}

func (r *base64PaddedReader) Read(buffer []byte) (int, error) {
	bytesRead, err := r.reader.Read(buffer)
	r.counter += bytesRead
	if err != io.EOF {
		return bytesRead, err
	}
	for bytesRead < len(buffer) {
		if r.counter%4 == 0 {
			return bytesRead, err
		}
		buffer[bytesRead] = byte(base64.StdPadding)
		bytesRead++
		r.counter++
	}
	return bytesRead, nil
}
