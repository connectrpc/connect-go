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
	"testing"
	"time"

	"github.com/bufbuild/connect-go/internal/assert"
)

func TestSubtractStatistics(t *testing.T) {
	t.Parallel()
	start := Statistics{
		Messages:         5,
		WireSize:         5 * 10,
		UncompressedSize: 5 * 20,
		Latency:          5 * 100 * time.Millisecond,
	}
	end := Statistics{
		Messages:         7,
		WireSize:         7 * 10,
		UncompressedSize: 7 * 20,
		Latency:          7 * 100 * time.Millisecond,
	}
	expect := Statistics{
		Messages:         2,
		WireSize:         2 * 10,
		UncompressedSize: 2 * 20,
		Latency:          2 * 100 * time.Millisecond,
	}
	assert.Equal(t, end.Sub(start), expect)
}
