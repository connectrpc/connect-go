package connect

import (
	"strings"
	"testing"

	"github.com/bufbuild/connect/internal/assert"
)

func TestCodeMarshaling(t *testing.T) {
	var valid []Code
	for code := minCode; code <= maxCode; code++ {
		valid = append(valid, code)
	}

	unmarshal := func(t testing.TB, code *Code, text []byte) {
		t.Helper()
		assert.Nil(t, code.UnmarshalText(text), "unmarshal Code from %q", assert.Fmt(text))
	}

	t.Run("round-trip", func(t *testing.T) {
		for _, code := range valid {
			text, err := code.MarshalText()
			assert.Nil(t, err, "marshal code %v", assert.Fmt(code))
			var in Code
			unmarshal(t, &in, text)
			assert.Equal(t, in, code, "round-trip failed")
		}
	})

	t.Run("out of bounds", func(t *testing.T) {
		const tooBig = maxCode + 1
		_, err := Code(tooBig).MarshalText()
		assert.NotNil(t, err, "marshal invalid code")
		_ = Code(tooBig).String() // shouldn't panic, output doesn't matter
		var code Code
		assert.NotNil(t, code.UnmarshalText([]byte("999")), "unmarshal out-of-bounds code")
		assert.NotNil(t, code.UnmarshalText([]byte("foobar")), "unmarshal invalid code")
	})

	t.Run("from string", func(t *testing.T) {
		var code Code
		unmarshal(t, &code, []byte("UNIMPLEMENTED"))
		assert.Equal(t, code, CodeUnimplemented, "unmarshal from text")
	})

	t.Run("to string", func(t *testing.T) {
		// Ensures that we don't forget to update the mapping in the Stringer
		// implementation.
		for _, code := range valid {
			assert.False(
				t,
				strings.Contains(code.String(), "("),
				"update Code.String() method for new codes!",
			)
		}
	})
}
