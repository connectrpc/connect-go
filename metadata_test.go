package rerpc

import (
	"context"
	"net/http"
	"testing"

	"github.com/akshayjshah/rerpc/internal/assert"
)

func TestCallMetadata(t *testing.T) {
	_, ok := CallMeta(context.Background())
	assert.False(t, ok, "no call metadata on bare context")

	spec := &Specification{
		Method:              "foo.v1.Foo/Bar",
		ContentType:         TypeJSON,
		RequestCompression:  CompressionIdentity,
		ResponseCompression: CompressionIdentity,
	}
	req, res := make(http.Header), make(http.Header)
	ctx := NewCallContext(context.Background(), *spec, req, res)
	md, ok := CallMeta(ctx)
	assert.True(t, ok, "get call metadata")
	assert.Equal(t, md.Spec.ContentType, TypeJSON, "content type")
	md.Spec.ContentType = TypeDefaultGRPC // only mutates our copy
	assert.Equal(t, spec.ContentType, TypeJSON, "specification should be value")
	md.Request.Set("Foo-Bar", "baz")
	assert.Equal(t, req, http.Header{"Foo-Bar": []string{"baz"}}, "request header after write")
}

func TestHandlerMetadata(t *testing.T) {
	_, ok := HandlerMeta(context.Background())
	assert.False(t, ok, "no handler metadata on bare context")

	spec := &Specification{
		Method:              "foo.v1.Foo/Bar",
		ContentType:         TypeJSON,
		RequestCompression:  CompressionIdentity,
		ResponseCompression: CompressionIdentity,
	}
	req, res := make(http.Header), make(http.Header)
	ctx := NewHandlerContext(context.Background(), *spec, req, res)
	md, ok := HandlerMeta(ctx)
	assert.True(t, ok, "get handler metadata")
	assert.Equal(t, md.Spec.ContentType, TypeJSON, "content type")
	md.Spec.ContentType = TypeDefaultGRPC // only mutates our copy
	assert.Equal(t, spec.ContentType, TypeJSON, "specification should be value")
	md.Response.Set("Foo-Bar", "baz")
	assert.Equal(t, res, http.Header{"Foo-Bar": []string{"baz"}}, "response header after write")
}
