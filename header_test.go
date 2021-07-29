package rerpc

import (
	"bytes"
	"math/rand"
	"testing"
	"testing/quick"
	"unicode"
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

func TestIsReservedHeader(t *testing.T) {
	tests := []struct {
		key      string
		reserved bool
	}{
		{"Accept", true},
		{"Accept-Encoding", true},
		{"Accept-Post", true},
		{"Allow", true},
		{"Content-Encoding", true},
		{"Content-Type", true},
		{"Te", true},
		{"Grpc-Foo", true},
		{"Rerpc-Foo", true},
		{"Twirp-Foo", true},

		{"Content-Length", false},
		{"Transfer-Encoding", false},
		{"Grpcfoo", false},
		{"Rerpcfoo", false},
		{"Twirpfoo", false},
		{"Google-Cloud-Trace-Id", false},
	}
	for _, tt := range tests {
		// Should be case-insensitive
		bs := []byte(tt.key)
		for i := 0; i < 10; i++ {
			if i > 0 {
				idx := rand.Intn(len(bs))
				r := rune(bs[idx])
				if unicode.IsLower(r) {
					bs[idx] = byte(unicode.ToUpper(r))
				} else {
					bs[idx] = byte(unicode.ToLower(r))
				}
			}
			k := string(bs)
			err := IsReservedHeader(k)
			if tt.reserved {
				assert.NotNil(t, err, "expected key %q to be reserved", assert.Fmt(k))
			} else {
				assert.Nil(t, err, "expected key %q to be available for application use", assert.Fmt(k))
			}
		}
	}
}
