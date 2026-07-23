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

package main

import (
	"strings"
	"testing"
)

// TestHandlerGroupSplitWarning checks that consecutive handlers whose options
// differ (and so cannot share one *connect.Server, since v2 interceptors apply
// per server) produce a warning asking the user to review the grouping.
func TestHandlerGroupSplitWarning(t *testing.T) {
	t.Parallel()
	src := `package example

import (
	"net/http"

	"connectrpc.com/connect"
	"connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/validate"
)

type PingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func run() error {
	checker := grpchealth.NewStaticChecker(pingv1connect.PingServiceName)
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(
		&PingServer{},
		connect.WithInterceptors(validate.NewInterceptor()),
	))
	mux.Handle(grpchealth.NewHandler(checker))
	return http.ListenAndServe(":8080", mux)
}
`
	_, report, err := Rewrite("input.go", []byte(src), true)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	found := false
	for _, warning := range report.Warnings {
		if warning.Rule == ruleHandlerConstruction && strings.Contains(warning.Msg, "differing options") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a %s warning about differing options; got %v", ruleHandlerConstruction, report.Warnings)
	}
}

// TestOptionTypeRenameDeclined checks the T2.2 guard: an option-slice helper
// that carries an interceptor is not HTTP-only, so its []connect.HandlerOption
// type is left unchanged (no option_type_to_connecthttp rename) and the existing
// reshape warning still fires, sending the human to move the interceptor.
func TestOptionTypeRenameDeclined(t *testing.T) {
	t.Parallel()
	src := `package p

import "connectrpc.com/connect"

func opts(ic connect.Interceptor) []connect.HandlerOption {
	return []connect.HandlerOption{connect.WithReadMaxBytes(1), connect.WithInterceptors(ic)}
}
`
	got, report, err := Rewrite("input.go", []byte(src), true)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !strings.Contains(string(got), "[]connect.HandlerOption") {
		t.Errorf("expected []connect.HandlerOption to be left unchanged; got:\n%s", got)
	}
	if count, ok := report.Counts["option_type_to_connecthttp"]; ok {
		t.Errorf("expected no option_type_to_connecthttp rename; got %d", count)
	}
	found := false
	for _, warning := range report.Warnings {
		if warning.Rule == ruleConnectHTTPOption || warning.Rule == ruleServerInterceptor {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a reshape warning for the interceptor-bearing options; got %v", report.Warnings)
	}
}

// TestReshapedSymbolWarnings checks that v1 symbols whose v2 home changed
// shape (so they can't be mechanically flipped) produce a warning naming the
// v2 destination rather than being silently left behind.
func TestReshapedSymbolWarnings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		body     string
		wantWarn string
	}{
		{
			name:     "new_error_detail",
			body:     `var _ = connect.NewErrorDetail`,
			wantWarn: "WithDetail",
		},
		{
			name:     "is_wire_error",
			body:     `var _ = connect.IsWireError`,
			wantWarn: "IsRemote",
		},
		{
			name:     "new_wire_error",
			body:     `var _ = connect.NewWireError`,
			wantWarn: "WithRemote",
		},
		{
			name:     "with_accept_compression",
			body:     `var _ = connect.WithAcceptCompression`,
			wantWarn: "connecthttp.WithCompressor",
		},
		{
			name:     "with_interceptors",
			body:     `var _ = connect.WithInterceptors`,
			wantWarn: "connect.NewServer",
		},
		{
			name:     "new_not_modified_error",
			body:     `var _ = connect.NewNotModifiedError`,
			wantWarn: "connecthttp.NewNotModifiedError",
		},
		{
			name:     "with_recover",
			body:     `var _ = connect.WithRecover`,
			wantWarn: "connect.ServerInterceptor that recovers panics",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			src := "package p\n\nimport \"connectrpc.com/connect\"\n\n" + test.body + "\n"
			_, report, err := Rewrite("input.go", []byte(src), true)
			if err != nil {
				t.Fatalf("Rewrite: %v", err)
			}
			found := false
			for _, warning := range report.Warnings {
				if strings.Contains(warning.Msg, test.wantWarn) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected a warning containing %q; got %v", test.wantWarn, report.Warnings)
			}
		})
	}
}

// TestMovedSymbolRewrites checks that v1 symbols that relocated to connecthttp
// verbatim (only the package qualifier changes) are rewritten mechanically
// instead of warned.
func TestMovedSymbolRewrites(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "new_error_writer",
			body: `var _ = connect.NewErrorWriter`,
			want: "connecthttp.NewErrorWriter",
		},
		{
			name: "error_writer_type",
			body: `var _ *connect.ErrorWriter`,
			want: "*connecthttp.ErrorWriter",
		},
		{
			name: "is_not_modified_error",
			body: `var _ = connect.IsNotModifiedError`,
			want: "connecthttp.IsNotModifiedError",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			src := "package p\n\nimport \"connectrpc.com/connect\"\n\n" + test.body + "\n"
			out, report, err := Rewrite("input.go", []byte(src), true)
			if err != nil {
				t.Fatalf("Rewrite: %v", err)
			}
			if !strings.Contains(string(out), test.want) {
				t.Errorf("expected output containing %q; got:\n%s", test.want, out)
			}
			if len(report.Warnings) != 0 {
				t.Errorf("expected no warnings; got %v", report.Warnings)
			}
		})
	}
}
