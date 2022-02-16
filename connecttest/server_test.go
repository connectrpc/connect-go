package connecttest

import (
	"net/http"
	"testing"

	"github.com/bufconnect/connect/internal/assert"
)

func TestServer(t *testing.T) {
	var called bool
	server := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.ProtoMajor, 2, "expected HTTP/2")
		assert.NotNil(t, r.TLS, "expected TLS")
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := server.Client()
	res, err := client.Get(server.URL())
	assert.Nil(t, err, "get error")
	assert.True(t, called, "handler not called")
	assert.Equal(t, res.StatusCode, http.StatusOK, "response status")
}
