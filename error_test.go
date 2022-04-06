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

	"github.com/bufbuild/connect-go/internal/assert"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestErrorNilUnderlying(t *testing.T) {
	t.Parallel()
	err := NewError(CodeUnknown, nil)
	assert.NotNil(t, err)
	assert.Equal(t, err.Error(), CodeUnknown.String())
	assert.Equal(t, err.Code(), CodeUnknown)
	assert.Zero(t, err.Details())
	detail, anyErr := anypb.New(&emptypb.Empty{})
	assert.Nil(t, anyErr)
	err.AddDetail(detail)
	assert.Equal(t, len(err.Details()), 1)
	err.Meta().Set("foo", "bar")
	assert.Equal(t, err.Meta().Get("foo"), "bar")
	assert.Equal(t, CodeOf(err), CodeUnknown)
}

func TestErrorFormatting(t *testing.T) {
	t.Parallel()
	assert.Equal(
		t,
		NewError(CodeUnavailable, errors.New("")).Error(),
		CodeUnavailable.String(),
	)
	got := NewError(CodeUnavailable, errors.New("foo")).Error()
	assert.False(t, strings.Contains(got, CodeUnavailable.String()))
	assert.True(t, strings.Contains(got, "foo"))
}

func TestErrorCode(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf(
		"another: %w",
		NewError(CodeUnavailable, errors.New("foo")),
	)
	connectErr, ok := asError(err)
	assert.True(t, ok)
	assert.Equal(t, connectErr.Code(), CodeUnavailable)
}

func TestCodeOf(t *testing.T) {
	t.Parallel()
	assert.Equal(
		t,
		CodeOf(NewError(CodeUnavailable, errors.New("foo"))),
		CodeUnavailable,
	)
	assert.Equal(t, CodeOf(errors.New("foo")), CodeUnknown)
}

func TestErrorDetails(t *testing.T) {
	t.Parallel()
	second := durationpb.New(time.Second)
	detail, err := anypb.New(second)
	assert.Nil(t, err)
	connectErr := NewError(CodeUnknown, errors.New("error with details"))
	assert.Zero(t, connectErr.Details())
	connectErr.AddDetail(detail)
	assert.Equal(t, connectErr.Details(), []ErrorDetail{detail})
}

func TestErrorIs(t *testing.T) {
	t.Parallel()
	// errors.New and fmt.Errorf return *errors.errorString. errors.Is
	// considers two *errors.errorStrings equal iff they have the same address.
	err := errors.New("oh no")
	assert.False(t, errors.Is(err, errors.New("oh no")))
	assert.True(t, errors.Is(err, err))
	// Our errors should have the same semantics. Note that we'd need to extend
	// the ErrorDetail interface to support value equality.
	connectErr := NewError(CodeUnavailable, err)
	assert.False(t, errors.Is(connectErr, NewError(CodeUnavailable, err)))
	assert.True(t, errors.Is(connectErr, connectErr))
}
