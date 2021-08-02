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

func TestHeaderWrappers(t *testing.T) {
	res, resV := "Content-Encoding", "gzip"
	unres, unresV := "Foo-Id", "barbaz"
	raw := http.Header{
		res:   []string{resV},
		unres: []string{unresV},
	}
	h := NewMutableHeader(raw)

	assert.Equal(t, h.Get(res), resV, "get reserved header")
	assert.Equal(t, h.Get(unres), unresV, "get unreserved header")

	vals := h.Values(res)
	assert.Equal(t, vals, []string{resV}, "values reserved header")
	vals[0] = "mutation should be safe"
	assert.Equal(t, h.Values(res), []string{resV}, "values after mutating returned slice")

	assert.Equal(t, h.Clone(), raw, "clone")

	assert.NotNil(t, h.Set(res, "foo"), "set reserved key")
	assert.NotNil(t, h.Add(res, "foo"), "add reserved key")
	assert.NotNil(t, h.Del(res), "delete reserved key")

	const k, v1, v2 = "Foo-Timeout", "one", "two"
	assert.Nil(t, h.Set(k, v1), "set unreserved key")
	assert.Equal(t, h.Get(k), v1, "get mutated unreserved key")
	assert.Nil(t, h.Add(k, v2), "add unreserved key")
	assert.Equal(t, h.Values(k), []string{v1, v2}, "values mutated unreserved key")
	assert.Nil(t, h.Del(k), "delete unreserved key")
	assert.Zero(t, h.Get(k), "get deleted key")

	const binary = "foo bar baz"
	assert.Nil(t, h.SetBinary(k, []byte(binary)), "set binary header")
	encoded := raw.Get(k + "-Bin")
	assert.NotZero(t, encoded, "base64-encoded value")
	assert.NotEqual(t, encoded, binary, "base64-encoded value")
	decoded, err := h.GetBinary(k)
	assert.Nil(t, err, "decode binary header")
	assert.Equal(t, string(decoded), binary, "round-trip binary header")
}
