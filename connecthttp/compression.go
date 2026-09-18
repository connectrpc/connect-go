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

package connecthttp

import (
	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/internal/compression"
)

// Compression pooling lives in internal/compression so that non-HTTP
// transports can share it. These aliases keep the in-package spellings.
type (
	compressionPool          = compression.Pool
	readOnlyCompressionPools = compression.ReadOnlyPools
)

func newReadOnlyCompressionPools(compressors []connect.Compressor) readOnlyCompressionPools {
	return compression.NewReadOnlyPools(compressors)
}
