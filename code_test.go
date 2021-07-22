package rerpc

import (
	"net/http"
	"strings"
	"testing"

	"github.com/akshayjshah/rerpc/internal/assert"
)

func TestCodeMarshaling(t *testing.T) {
	valid := make([]Code, 0)
	for c := minCode; c <= maxCode; c++ {
		valid = append(valid, c)
	}

	unmarshal := func(t testing.TB, c *Code, text []byte) {
		assert.Nil(t, c.UnmarshalText(text), "unmarshal Code from %q", assert.Fmt(text))
	}

	t.Run("round-trip", func(t *testing.T) {
		for _, c := range valid {
			out, err := c.MarshalText()
			assert.Nil(t, err, "marshal code %v", assert.Fmt(c))
			var in Code
			unmarshal(t, &in, out)
			assert.Equal(t, in, c, "round-trip failed")
		}
	})

	t.Run("out of bounds", func(t *testing.T) {
		_, err := Code(42).MarshalText()
		assert.NotNil(t, err, "marshal invalid code")
		_ = Code(42).String() // shouldn't panic, output doesn't matter
		c := new(Code)
		assert.NotNil(t, c.UnmarshalText([]byte("42")), "unmarshal out-of-bounds code")
		assert.NotNil(t, c.UnmarshalText([]byte("foobar")), "unmarshal invalid code")
	})

	t.Run("from string", func(t *testing.T) {
		var c Code
		unmarshal(t, &c, []byte("UNIMPLEMENTED"))
		assert.Equal(t, c, CodeUnimplemented, "unmarshal from text")
	})

	t.Run("to string", func(t *testing.T) {
		for _, c := range valid {
			assert.False(t, strings.Contains(c.String(), "("), "update String() method for new Codes!")
		}
	})
}

func TestCodeHTTPMapping(t *testing.T) {
	assert.Equal(t, Code(999).http(), http.StatusInternalServerError, "out-of-bounds code")
	assert.Equal(t, CodeOK.http(), http.StatusOK, "code OK")
}
