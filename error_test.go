package connect

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/bufconnect/connect/internal/assert"
)

func TestErrorCodeOK(t *testing.T) {
	assert.Nil(t, Wrap(CodeOK, errors.New("ok")), "wrap code ok")
	assert.Nil(t, Errorf(CodeOK, "ok"), "errorf code ok")
}

func TestErrorFormatting(t *testing.T) {
	assert.Equal(
		t,
		Errorf(CodeUnavailable, "").Error(),
		CodeUnavailable.String(),
		"no message",
	)
	got := Errorf(CodeUnavailable, "foo").Error()
	assert.True(t, strings.Contains(got, CodeUnavailable.String()), "error text should include code")
	assert.True(t, strings.Contains(got, "foo"), "error text should include message")
}

func TestErrorCode(t *testing.T) {
	err := fmt.Errorf("another: %w", Errorf(CodeUnavailable, "foo"))
	connectErr, ok := AsError(err)
	assert.True(t, ok, "extract connect error")
	assert.Equal(t, connectErr.Code(), CodeUnavailable, "extracted code")
}

func TestCodeOf(t *testing.T) {
	assert.Equal(t, CodeOf(nil), CodeOK, "nil error code")
	assert.Equal(t, CodeOf(Errorf(CodeUnavailable, "foo")), CodeUnavailable, "explicitly-set code")
	assert.Equal(t, CodeOf(errors.New("foo")), CodeUnknown, "fallback code")
}

func TestErrorDetails(t *testing.T) {
	second := durationpb.New(time.Second)
	detail, err := anypb.New(second)
	assert.Nil(t, err, "create anypb.Any")
	connectErr := Errorf(CodeUnknown, "error with details")
	assert.Zero(t, connectErr.Details(), "details before adding")
	connectErr.AddDetail(detail)
	assert.Equal(t, connectErr.Details(), []ErrorDetail{detail}, "details after adding")
}
