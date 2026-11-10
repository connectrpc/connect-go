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

package wstransport

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect/v2/internal/assert"
)

func TestDrainLimiter(t *testing.T) {
	t.Parallel()
	limiter := newDrainLimiter(10)
	reader := strings.NewReader(strings.Repeat("x", 100))
	buf := make([]byte, 64)

	// Reads before the drain are not counted.
	n, err := limiter.read(reader, buf)
	assert.Nil(t, err)
	assert.Equal(t, n, 64)

	limiter.begin()
	n, err = limiter.read(reader, buf)
	assert.Nil(t, err)
	assert.Equal(t, n, 10)
	_, err = limiter.read(reader, buf)
	assert.True(t, errors.Is(err, errDrainBudgetSpent))

	// A second begin does not refill the budget.
	limiter.begin()
	_, err = limiter.read(reader, buf)
	assert.True(t, errors.Is(err, errDrainBudgetSpent))
}

func TestDrainLimiterBudget(t *testing.T) {
	t.Parallel()
	assert.Equal(t, newDrainLimiter(messageReadLimit(1024)).budget, int64(1025))
	assert.Equal(t, newDrainLimiter(messageReadLimit(0)).budget, int64(defaultDrainBudget))

	var limiter *drainLimiter
	limiter.begin()
	n, err := limiter.read(strings.NewReader("abc"), make([]byte, 8))
	assert.Nil(t, err)
	assert.Equal(t, n, 3)
}
