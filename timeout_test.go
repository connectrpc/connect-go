package rerpc

import (
	"math"
	"testing"
	"time"

	"github.com/akshayjshah/rerpc/internal/assert"
)

func TestParseTimeout(t *testing.T) {
	_, err := parseTimeout("")
	assert.True(t, err == errNoTimeout, "expect errNoTimeout for empty string")

	_, err = parseTimeout("foo")
	assert.NotNil(t, err, "foo")
	_, err = parseTimeout("12xS")
	assert.NotNil(t, err, "12xS")
	_, err = parseTimeout("999999999n") // 9 digits
	assert.NotNil(t, err, "too many digits")
	assert.False(t, err == errNoTimeout, "too many digits")
	_, err = parseTimeout("99999999H") // 8 digits but overflows time.Duration
	assert.True(t, err == errNoTimeout, "effectively unbounded")

	d, err := parseTimeout("45S")
	assert.Nil(t, err, "45S")
	assert.Equal(t, d, 45*time.Second, "45S")
}

func TestEncodeTimeout(t *testing.T) {
	to, err := encodeTimeout(time.Hour + time.Second)
	assert.Nil(t, err, "1h1s")
	assert.Equal(t, to, "3601000m", "1h1s")
	to, err = encodeTimeout(time.Duration(math.MaxInt64))
	assert.Nil(t, err, "max duration")
	assert.Equal(t, to, "2562047H", "max duration")
}
