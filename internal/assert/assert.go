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
func Equal[T any](t testing.TB, got, want T, message string) bool {
	t.Helper()
	if cmpEqual(got, want) {
		return true
	}
	report(t, got, want, message, "assert.Equal", true /* showWant */)
	return false
}

// NotEqual asserts that two values aren't equal.
func NotEqual[T any](t testing.TB, got, want T, message string) bool {
	t.Helper()
	if !cmpEqual(got, want) {
		return true
	}
	report(t, got, want, message, "assert.NotEqual", true /* showWant */)
	return false
}

// Nil asserts that the value is nil.
func Nil(t testing.TB, got any, message string) bool {
	t.Helper()
	if isNil(got) {
		return true
	}
	report(t, got, nil, message, "assert.Nil", false /* showWant */)
	return false
}

// NotNil asserts that the value isn't nil.
func NotNil(t testing.TB, got any, message string) bool {
	t.Helper()
	if !isNil(got) {
		return true
	}
	report(t, got, nil, message, "assert.NotNil", false /* showWant */)
	return false
}

// Zero asserts that the value is its type's zero value.
func Zero[T any](t testing.TB, got T, message string) bool {
	t.Helper()
	var want T
	if cmpEqual(got, want) {
		return true
	}
	report(t, got, want, message, fmt.Sprintf("assert.Zero (type %T)", got), false /* showWant */)
	return false
}

// NotZero asserts that the value is non-zero.
func NotZero[T any](t testing.TB, got T, message string) bool {
	t.Helper()
	var want T
	if !cmpEqual(got, want) {
		return true
	}
	report(t, got, want, message, fmt.Sprintf("assert.NotZero (type %T)", got), false /* showWant */)
	return false
}

// Match asserts that the value matches a regexp.
func Match(t testing.TB, got, want, message string) bool {
	t.Helper()
	re, err := regexp.Compile(want)
	if err != nil {
		t.Fatalf("invalid regexp %q: %v", want, err)
	}
	if re.MatchString(got) {
		return true
	}
	report(t, got, want, message, "assert.Match", true /* showWant */)
	return false
}

// ErrorIs asserts that "want" is in "got's" error chain. See the standard
// library's errors package for details on error chains. On failure, output is
// identical to Equal.
func ErrorIs(t testing.TB, got, want error, message string) bool {
	t.Helper()
	if errors.Is(got, want) {
		return true
	}
	report(t, got, want, message, "assert.ErrorIs", true /* showWant */)
	return false
}

// False asserts that "got" is false.
func False(t testing.TB, got bool, message string) bool {
	t.Helper()
	if !got {
		return true
	}
	report(t, got, false, message, "assert.False", false /* showWant */)
	return false
}

// True asserts that "got" is true.
func True(t testing.TB, got bool, message string) bool {
	t.Helper()
	if got {
		return true
	}
	report(t, got, true, message, "assert.True", false /* showWant */)
	return false
}

// Panics asserts that the function called panics.
func Panics(t testing.TB, panicker func(), message string) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			report(t, r, nil, message, "assert.Panic", false /* showWant */)
		}
	}()
	panicker()
}

func report(t testing.TB, got, want any, message, desc string, showWant bool) {
	t.Helper()
	w := &bytes.Buffer{}
	if message != "" {
		w.WriteString(message)
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
