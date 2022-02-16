package connect

import (
	"bytes"
	"net/http"
	"testing"
	"testing/quick"
	"unicode/utf8"

	"github.com/bufconnect/connect/internal/assert"
)

func TestBinaryEncodingQuick(t *testing.T) {
	roundtrip := func(bs []byte) bool {
		encoded := EncodeBinaryHeader(bs)
		decoded, err := DecodeBinaryHeader(encoded)
		if err != nil {
			// We want to abort immediately. Don't use our assert package.
			t.Fatalf("decode error: %v", err)
		}
		return bytes.Equal(decoded, bs)
	}
	if err := quick.Check(roundtrip, nil /* config */); err != nil {
		t.Error(err)
	}
}

func TestPercentEncodingQuick(t *testing.T) {
	roundtrip := func(input string) bool {
		if !utf8.ValidString(input) {
			return true
		}
		encoded := percentEncode(input)
		decoded := percentDecode(encoded)
		return decoded == input
	}
	if err := quick.Check(roundtrip, nil /* config */); err != nil {
		t.Error(err)
	}
}

func TestPercentEncoding(t *testing.T) {
	roundtrip := func(input string) {
		assert.True(t, utf8.ValidString(input), "input invalid UTF-8")
		encoded := percentEncode(input)
		t.Logf("%q encoded as %q", input, encoded)
		decoded := percentDecode(encoded)
		assert.Equal(t, decoded, input, "roundtrip corrupted string")
	}

	roundtrip("foo")
	roundtrip("foo bar")
	roundtrip(`foo%bar`)
	roundtrip("fiancée")
}

func TestHeaderMerge(t *testing.T) {
	header := http.Header{
		"Foo": []string{"one"},
	}
	mergeHeaders(header, http.Header{
		"Foo": []string{"two"},
		"Bar": []string{"one"},
		"Baz": nil,
	})
	expect := http.Header{
		"Foo": []string{"one", "two"},
		"Bar": []string{"one"},
		"Baz": nil,
	}
	assert.Equal(t, header, expect, "merge result")
}
