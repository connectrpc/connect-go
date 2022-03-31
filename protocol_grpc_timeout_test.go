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
	"errors"
	"math"
	"testing"
	"testing/quick"
	"time"

	"connectrpc.com/connect/internal/assert"
)

func TestParseTimeout(t *testing.T) {
	t.Parallel()
	_, err := parseTimeout("")
	assert.True(t, errors.Is(err, errNoTimeout))

	_, err = parseTimeout("foo")
	assert.NotNil(t, err)
	_, err = parseTimeout("12xS")
	assert.NotNil(t, err)
	_, err = parseTimeout("999999999n") // 9 digits
	assert.NotNil(t, err)
	assert.False(t, errors.Is(err, errNoTimeout))
	_, err = parseTimeout("99999999H") // 8 digits but overflows time.Duration
	assert.True(t, errors.Is(err, errNoTimeout))

	duration, err := parseTimeout("45S")
	assert.Nil(t, err)
	assert.Equal(t, duration, 45*time.Second)

	const long = "99999999S"
	duration, err = parseTimeout(long) // 8 digits, shouldn't overflow
	assert.Nil(t, err)
	assert.Equal(t, duration, 99999999*time.Second)
}

func TestEncodeTimeout(t *testing.T) {
	t.Parallel()
	timeout, err := encodeTimeout(time.Hour + time.Second)
	assert.Nil(t, err)
	assert.Equal(t, timeout, "3601000m")
	timeout, err = encodeTimeout(time.Duration(math.MaxInt64))
	assert.Nil(t, err)
	assert.Equal(t, timeout, "2562047H")
	timeout, err = encodeTimeout(-1 * time.Hour)
	assert.Nil(t, err)
	assert.Equal(t, timeout, "0n")
}

func TestEncodeTimeoutQuick(t *testing.T) {
	t.Parallel()
	// Ensure that the error case is actually unreachable.
	encode := func(d time.Duration) bool {
		_, err := encodeTimeout(d)
		return err == nil
	}
	if err := quick.Check(encode, nil); err != nil {
		t.Error(err)
	}
}
