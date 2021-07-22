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

type params struct {
	got, want interface{}
	cmpOpts   []cmp.Option // user-supplied equality configuration
	msg       string       // user-supplied description of failure
	diff      bool         // include diff in failure output
}

func newParams(got, want interface{}, msg string, opts ...Option) *params {
	p := &params{
		got:     got,
		want:    want,
		cmpOpts: []cmp.Option{protocmp.Transform()},
		msg:     msg,
	}
	for _, opt := range opts {
		opt.apply(p)
	}
	if p.got == nil || p.want == nil {
		// diff panics on nil
		p.diff = false
	}
	return p
}

// An Option modifies assertions.
type Option interface {
	apply(*params)
}

type optionFunc func(*params)

func (f optionFunc) apply(p *params) { f(p) }

// Cmp adds cmp.Options to the assertion.
func Cmp(opts ...cmp.Option) Option {
	return optionFunc(func(p *params) {
		p.cmpOpts = append(p.cmpOpts, opts...)
	})
}

// Fmt treats the assertion message as a template, using fmt.Sprintf and the
// supplied arguments to expand it. For example,
//   assert.Equal(t, 0, 1, "failed to parse %q", assert.Fmt("foobar"))
// will print the message
//   failed to parse "foobar"
func Fmt(args ...interface{}) Option {
	return optionFunc(func(p *params) {
		if len(args) > 0 {
			p.msg = fmt.Sprintf(p.msg, args...)
		}
	})
}

// Diff prints a diff between "got" and "want" on failures.
func Diff() Option {
	return optionFunc(func(p *params) {
		p.diff = true
	})
}

// Equal asserts that two values are equal.
func Equal(t testing.TB, got, want interface{}, msg string, opts ...Option) bool {
	t.Helper()
	params := newParams(got, want, msg, opts...)
	if cmp.Equal(got, want, params.cmpOpts...) {
		return true
	}
	report(t, params, "assert.Equal", true /* showWant */)
	return false
}

// NotEqual asserts that two values aren't equal.
func NotEqual(t testing.TB, got, want interface{}, msg string, opts ...Option) bool {
	t.Helper()
	params := newParams(got, want, msg, opts...)
	if !cmp.Equal(got, want, params.cmpOpts...) {
		return true
	}
	report(t, params, "assert.NotEqual", true /* showWant */)
	return false
}

// Nil asserts that the value is nil.
func Nil(t testing.TB, got interface{}, msg string, opts ...Option) bool {
	t.Helper()
	if isNil(got) {
		return true
	}
	params := newParams(got, nil, msg, opts...)
	report(t, params, "assert.Nil", false /* showWant */)
	return false
}

// NotNil asserts that the value isn't nil.
func NotNil(t testing.TB, got interface{}, msg string, opts ...Option) bool {
	t.Helper()
	if !isNil(got) {
		return true
	}
	params := newParams(got, nil, msg, opts...)
	report(t, params, "assert.NotNil", false /* showWant */)
	return false
}

// Zero asserts that the value is its type's zero value.
func Zero(t testing.TB, got interface{}, msg string, opts ...Option) bool {
	t.Helper()
	if got == nil {
		return true
	}
	want := makeZero(got)
	params := newParams(got, want, msg, opts...)
	if cmp.Equal(got, want, params.cmpOpts...) {
		return true
	}
	report(t, params, fmt.Sprintf("assert.Zero (type %T)", got), false /* showWant */)
	return false
}

// NotZero asserts that the value is non-zero.
func NotZero(t testing.TB, got interface{}, msg string, opts ...Option) bool {
	t.Helper()
	if got != nil {
		want := makeZero(got)
		params := newParams(got, want, msg, opts...)
		if !cmp.Equal(got, want, params.cmpOpts...) {
			return true
		}
	}
	params := newParams(got, nil, msg, opts...)
	report(t, params, fmt.Sprintf("assert.NotZero (type %T)", got), false /* showWant */)
	return false
}

// Match asserts that the value matches a regexp.
func Match(t testing.TB, got, want string, msg string, opts ...Option) bool {
	t.Helper()
	re, err := regexp.Compile(want)
	if err != nil {
		t.Fatalf("invalid regexp %q: %v", want, err)
	}
	if re.MatchString(got) {
		return true
	}
	params := newParams(got, want, msg, opts...)
	report(t, params, "assert.Match", true /* showWant */)
	return false
}

// ErrorIs asserts that "want" is in "got's" error chain. See the standard
// library's errors package for details on error chains. On failure, output is
// identical to Equal.
func ErrorIs(t testing.TB, got, want error, msg string, opts ...Option) bool {
	t.Helper()
	if errors.Is(got, want) {
		return true
	}
	params := newParams(got, want, msg, opts...)
	report(t, params, "assert.ErrorIs", true /* showWant */)
	return false
}

// False asserts that "got" is false.
func False(t testing.TB, got bool, msg string, opts ...Option) bool {
	t.Helper()
	if !got {
		return true
	}
	params := newParams(got, false, msg, opts...)
	report(t, params, "assert.False", false)
	return false
}

// True asserts that "got" is true.
func True(t testing.TB, got bool, msg string, opts ...Option) bool {
	t.Helper()
	if got {
		return true
	}
	params := newParams(got, false, msg, opts...)
	report(t, params, "assert.True", false)
	return false
}

// Panics asserts that the function called panics.
func Panics(t testing.TB, panicker func(), msg string, opts ...Option) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			params := newParams("no panic", "panic", msg, opts...)
			report(t, params, "assert.Panic", false)
		}
	}()
	panicker()
}

func report(t testing.TB, params *params, desc string, showWant bool) {
	t.Helper()
	w := &bytes.Buffer{}
	if params.msg != "" {
		w.WriteString(params.msg)
	}
	w.WriteString("\n")
	fmt.Fprintf(w, "assertion:\t%s\n", desc)
	fmt.Fprintf(w, "got:\t%+v\n", params.got)
	if showWant {
		fmt.Fprintf(w, "want:\t%+v\n", params.want)
	}
	if params.diff {
		fmt.Fprintf(w, "\ndiff (-want, +got):\n%v", cmp.Diff(params.want, params.got))
	}
	t.Fatal(w.String())
}

func isNil(got interface{}) bool {
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

func makeZero(i interface{}) interface{} {
	typ := reflect.TypeOf(i)
	return reflect.Zero(typ).Interface()
}
