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

// Package assert is a minimal assert package using generics.
//
// This prevents connect from needing additional dependencies.
package assert

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
)

// Equal asserts that two values are equal.
func Equal[T any](t testing.TB, got, want T, options ...Option) bool {
	t.Helper()
	if cmpEqual(got, want) {
		return true
	}
	report(t, got, want, "assert.Equal", true /* showWant */, options...)
	return false
}

// NotEqual asserts that two values aren't equal.
func NotEqual[T any](t testing.TB, got, want T, options ...Option) bool {
	t.Helper()
	if !cmpEqual(got, want) {
		return true
	}
	report(t, got, want, "assert.NotEqual", true /* showWant */, options...)
	return false
}

// Nil asserts that the value is nil.
func Nil(t testing.TB, got any, options ...Option) bool {
	t.Helper()
	if isNil(got) {
		return true
	}
	report(t, got, nil, "assert.Nil", false /* showWant */, options...)
	return false
}

// NotNil asserts that the value isn't nil.
func NotNil(t testing.TB, got any, options ...Option) bool {
	t.Helper()
	if !isNil(got) {
		return true
	}
	report(t, got, nil, "assert.NotNil", false /* showWant */, options...)
	return false
}

// Zero asserts that the value is its type's zero value.
func Zero[T any](t testing.TB, got T, options ...Option) bool {
	t.Helper()
	var want T
	if cmpEqual(got, want) {
		return true
	}
	report(t, got, want, fmt.Sprintf("assert.Zero (type %T)", got), false /* showWant */, options...)
	return false
}

// NotZero asserts that the value is non-zero.
func NotZero[T any](t testing.TB, got T, options ...Option) bool {
	t.Helper()
	var want T
	if !cmpEqual(got, want) {
		return true
	}
	report(t, got, want, fmt.Sprintf("assert.NotZero (type %T)", got), false /* showWant */, options...)
	return false
}

// Match asserts that the value matches a regexp.
func Match(t testing.TB, got, want string, options ...Option) bool {
	t.Helper()
	re, err := regexp.Compile(want)
	if err != nil {
		t.Fatalf("invalid regexp %q: %v", want, err)
	}
	if re.MatchString(got) {
		return true
	}
	report(t, got, want, "assert.Match", true /* showWant */, options...)
	return false
}

// ErrorIs asserts that "want" is in "got's" error chain. See the standard
// library's errors package for details on error chains. On failure, output is
// identical to Equal.
func ErrorIs(t testing.TB, got, want error, options ...Option) bool {
	t.Helper()
	if errors.Is(got, want) {
		return true
	}
	report(t, got, want, "assert.ErrorIs", true /* showWant */, options...)
	return false
}

// False asserts that "got" is false.
func False(t testing.TB, got bool, options ...Option) bool {
	t.Helper()
	if !got {
		return true
	}
	report(t, got, false, "assert.False", false /* showWant */, options...)
	return false
}

// True asserts that "got" is true.
func True(t testing.TB, got bool, options ...Option) bool {
	t.Helper()
	if got {
		return true
	}
	report(t, got, true, "assert.True", false /* showWant */, options...)
	return false
}

// Panics asserts that the function called panics.
func Panics(t testing.TB, panicker func(), options ...Option) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			report(t, r, nil, "assert.Panic", false /* showWant */, options...)
		}
	}()
	panicker()
}

// An Option configures an assertion.
type Option interface {
	// Only option we've needed so far is a formatted message, so we can keep
	// this simple.
	message() string
}

// Sprintf adds a user-defined message to the assertion's output. The arguments
// are passed directly to fmt.Sprintf for formatting.
//
// If Sprintf is passed multiple times, only the last message is used.
func Sprintf(template string, args ...any) Option {
	return &sprintfOption{fmt.Sprintf(template, args...)}
}

type sprintfOption struct {
	msg string
}

func (o *sprintfOption) message() string {
	return o.msg
}

func report(t testing.TB, got, want any, desc string, showWant bool, options ...Option) {
	t.Helper()
	w := &bytes.Buffer{}
	if len(options) > 0 {
		w.WriteString(options[len(options)-1].message())
	}
	w.WriteString("\n")
	fmt.Fprintf(w, "assertion:\t%s\n", desc)
	fmt.Fprintf(w, "got:\t%+v\n", got)
	if showWant {
		fmt.Fprintf(w, "want:\t%+v\n", want)
	}
	t.Fatal(w.String())
}

func isNil(got any) bool {
	// Simple case, true only when the user directly passes a literal nil.
	if got == nil {
		return true
	}
	// Possibly more complex. Interfaces are a pair of words: a pointer to a type
	// and a pointer to a value. Because we're passing got as an interface, it's
	// likely that we've gotten a non-nil type and a nil value. This makes got
	// itself non-nil, but the user's code passed a nil value.
	val := reflect.ValueOf(got)
	switch val.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return val.IsNil()
	default:
		return false
	}
}

func cmpEqual(got, want any) bool {
	return cmp.Equal(got, want, protocmp.Transform())
}
