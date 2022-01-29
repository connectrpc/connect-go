package rerpc

import (
	"bytes"
	"net/http"
	"testing"
	"testing/quick"
	"unicode/utf8"

	"github.com/rerpc/rerpc/internal/assert"
)

func TestBinaryEncodingQuick(t *testing.T) {
	roundtrip := func(bs []byte) bool {
		encoded := EncodeBinaryHeader(bs)
		decoded, err := DecodeBinaryHeader(encoded)
		if err != nil {
			t.Fatalf("decode error: %v", err)
		}
		return bytes.Equal(decoded, bs)
	}
	if err := quick.Check(roundtrip, nil); err != nil {
		t.Error(err)
	}
}

func TestPercentEncodingQuick(t *testing.T) {
	roundtrip := func(s string) bool {
		if !utf8.ValidString(s) {
			return true
		}
		encoded := percentEncode(s)
		decoded := percentDecode(encoded)
		return decoded == s
	}
	if err := quick.Check(roundtrip, nil); err != nil {
		t.Error(err)
	}
}

func TestPercentEncoding(t *testing.T) {
	roundtrip := func(s string) {
		assert.True(t, utf8.ValidString(s), "input invalid UTF-8")
		encoded := percentEncode(s)
		t.Logf("%q encoded as %q", s, encoded)
		decoded := percentDecode(encoded)
		assert.Equal(t, decoded, s, "roundtrip corrupted string")
	}

	roundtrip("foo")
	roundtrip("foo bar")
	roundtrip(`foo%bar`)
	roundtrip("fiancée")
}

func TestHeaderMerge(t *testing.T) {
	h := http.Header{
		"Foo": []string{"one"},
	}
	mergeHeaders(h, http.Header{
		"Foo": []string{"two"},
		"Bar": []string{"one"},
		"Baz": nil,
	})
	expect := http.Header{
		"Foo": []string{"one", "two"},
		"Bar": []string{"one"},
		"Baz": nil,
	}
	assert.Equal(t, h, expect, "merge result")
}
