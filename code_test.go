package connect

import (
	"strings"
	"testing"

	"github.com/bufbuild/connect/internal/assert"
)

func TestCode(t *testing.T) {
	var valid []Code
	for code := minCode; code <= maxCode; code++ {
		valid = append(valid, code)
	}
	// Ensures that we don't forget to update the mapping in the Stringer
	// implementation.
	for _, code := range valid {
		assert.False(
			t,
			strings.Contains(code.String(), "("),
			"update Code.String() method for new codes!",
		)
	}
}
