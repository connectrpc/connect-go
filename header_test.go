package rerpc

import (
	"bytes"
	"math/rand"
	"net/http"
	"testing"
	"testing/quick"
	"unicode"
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

func TestIsValidHeaderKey(t *testing.T) {
	tests := []struct {
		key   string
		valid bool
	}{
		// Invalid characters
		{"", false},
		{"Foo\uF000", false},
		{"Foo$", false},
		{"Foo!", false},

		// HTTP2 proto-headers
		{":method", false},
		{":scheme", false},
		{":authority", false},
		{":path", false},
		{":foo", false},

		// Available
		{"Accept-Post", true},
		{"Grpc-Foo", true},
		{"Content-Length", true},
		{"Transfer-Encoding", true},
		{"Grpcfoo", true},
		{"Rerpcfoo", true},
		{"Google-Cloud-Trace-Id", true},
		{"Foo_bar", true},
		{"Foo.bar", true},
	}

	testHeaderKey := func(t testing.TB, name string, valid bool) {
		t.Helper()
		err := IsValidHeaderKey(name)
		if valid {
			assert.Nil(t, err, "expected key %q to be available for application use", assert.Fmt(name))
		} else {
			assert.NotNil(t, err, "expected key %q to be reserved", assert.Fmt(name))
		}
	}

	for _, tt := range tests {
		if len(tt.key) == 0 {
			testHeaderKey(t, tt.key, tt.valid)
			continue
		}
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
			testHeaderKey(t, k, tt.valid)
		}
	}
}

func TestIsValidHeaderValue(t *testing.T) {
	tests := []struct {
		s     string
		valid bool
	}{
		{"", true},
		{":authority", true},
		{"Authority", true},
		{"AutHoR-it!@$`", true},
		{"Grpc-Timeout", true},
		{"foo", true},
		{"foo\x00", false}, // ASCII null
		{"foo\x7F", false}, // ASCII delete
	}

	for _, tt := range tests {
		err := IsValidHeaderValue(tt.s)
		if tt.valid {
			assert.Nil(t, err, "expected %q to be valid", assert.Fmt(tt.s))
		} else {
			assert.NotNil(t, err, "expected %q to be invalid", assert.Fmt(tt.s))
		}
	}
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
