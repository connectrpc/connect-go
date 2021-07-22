package rerpc

import (
	"bytes"
	"testing"
	"testing/quick"
	"unicode/utf8"

	"github.com/akshayjshah/rerpc/internal/assert"
)

func TestBinaryEncodingQuick(t *testing.T) {
	roundtrip := func(bs []byte) bool {
		encoded := encodeBinaryHeader(bs)
		decoded, err := decodeBinaryHeader(encoded)
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
