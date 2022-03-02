// Copyright 2021-2022 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connect

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bufbuild/connect/internal/assert"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestErrorNilUnderlying(t *testing.T) {
	err := NewError(CodeUnknown, nil)
	assert.NotNil(t, err, "should be allowed to wrap a nil error")
	assert.Equal(t, err.Error(), CodeUnknown.String(), "message")
	assert.Equal(t, err.Code(), CodeUnknown, "code")
	assert.Zero(t, err.Details(), "details")
	detail, anyErr := anypb.New(&emptypb.Empty{})
	assert.Nil(t, anyErr, "create proto.Any")
	err.AddDetail(detail)
	assert.Equal(t, len(err.Details()), 1, "details")
	err.Header().Set("foo", "bar")
	assert.Equal(t, err.Header().Get("foo"), "bar", "header")
	err.Trailer().Set("baz", "quux")
	assert.Equal(t, err.Trailer().Get("baz"), "quux", "trailer")
	assert.Equal(t, CodeOf(err), CodeUnknown, "code")
}

func TestErrorFormatting(t *testing.T) {
	assert.Equal(
		t,
		NewError(CodeUnavailable, errors.New("")).Error(),
		CodeUnavailable.String(),
		"no message",
	)
	got := NewError(CodeUnavailable, errors.New("foo")).Error()
	assert.True(t, strings.Contains(got, CodeUnavailable.String()), "error text should include code")
	assert.True(t, strings.Contains(got, "foo"), "error text should include message")
}

func TestErrorCode(t *testing.T) {
	err := fmt.Errorf(
		"another: %w",
		NewError(CodeUnavailable, errors.New("foo")),
	)
	connectErr, ok := asError(err)
	assert.True(t, ok, "extract connect error")
	assert.Equal(t, connectErr.Code(), CodeUnavailable, "extracted code")
}

func TestCodeOf(t *testing.T) {
	assert.Equal(
		t,
		CodeOf(NewError(CodeUnavailable, errors.New("foo"))),
		CodeUnavailable,
		"explicitly-set code",
	)
	assert.Equal(t, CodeOf(errors.New("foo")), CodeUnknown, "fallback code")
}

func TestErrorDetails(t *testing.T) {
	second := durationpb.New(time.Second)
	detail, err := anypb.New(second)
	assert.Nil(t, err, "create anypb.Any")
	connectErr := NewError(CodeUnknown, errors.New("error with details"))
	assert.Zero(t, connectErr.Details(), "details before adding")
	connectErr.AddDetail(detail)
	assert.Equal(t, connectErr.Details(), []ErrorDetail{detail}, "details after adding")
}
