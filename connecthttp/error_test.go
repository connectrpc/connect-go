// Copyright 2021-2026 The Connect Authors
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

package connecthttp

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/connect/v2/internal/assert"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestErrorNilUnderlying(t *testing.T) {
	t.Parallel()
	err := connect.NewError(connect.CodeUnknown, "")
	assert.NotNil(t, err)
	assert.Equal(t, err.Error(), connect.CodeUnknown.String())
	assert.Equal(t, err.Code(), connect.CodeUnknown)
	assert.Zero(t, err.Details())
	detail, detailErr := connectproto.NewErrorDetail(&emptypb.Empty{})
	assert.Nil(t, detailErr)
	err = err.WithDetail(detail)
	assert.Equal(t, len(err.Details()), 1)
	anyDetail := connectproto.ErrorDetailToAny(err.Details()[0])
	assert.Equal(t, anyDetail.GetTypeUrl(), "type.googleapis.com/google.protobuf.Empty")
}

func TestErrorFormatting(t *testing.T) {
	t.Parallel()
	assert.Equal(
		t,
		connect.NewError(connect.CodeUnavailable, "").Error(),
		connect.CodeUnavailable.String(),
	)
	got := connect.NewError(connect.CodeUnavailable, "Foo").Error()
	assert.True(t, strings.Contains(got, connect.CodeUnavailable.String()))
	assert.True(t, strings.Contains(got, "Foo"))
}

func TestErrorCode(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf(
		"another: %w",
		connect.NewError(connect.CodeUnavailable, "foo"),
	)
	connectErr, ok := asError(err)
	assert.True(t, ok)
	assert.Equal(t, connectErr.Code(), connect.CodeUnavailable)
}

func TestCodeOf(t *testing.T) {
	t.Parallel()
	assert.Equal(
		t,
		connect.CodeOf(connect.NewError(connect.CodeUnavailable, "foo")),
		connect.CodeUnavailable,
	)
	assert.Equal(t, connect.CodeOf(errors.New("foo")), connect.CodeUnknown)
}

func TestErrorDetails(t *testing.T) {
	t.Parallel()
	second := durationpb.New(time.Second)
	detail, err := connectproto.NewErrorDetail(second)
	assert.Nil(t, err)
	connectErr := connect.NewError(connect.CodeUnknown, "error with details")
	assert.Zero(t, connectErr.Details())
	connectErr = connectErr.WithDetail(detail)
	assert.Equal(t, len(connectErr.Details()), 1)
	unmarshaled, err := connectproto.UnmarshalErrorDetail(connectErr.Details()[0])
	assert.Nil(t, err)
	assert.Equal(t, unmarshaled, proto.Message(second))
	gotAny := connectproto.ErrorDetailToAny(connectErr.Details()[0])
	secondBin, err := proto.Marshal(second)
	assert.Nil(t, err)
	assert.Equal(t, gotAny.Value, secondBin)
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
	connectErr := connect.NewError(connect.CodeUnavailable, err.Error())
	assert.False(t, errors.Is(connectErr, connect.NewError(connect.CodeUnavailable, err.Error())))
	assert.True(t, errors.Is(connectErr, connectErr))
}
