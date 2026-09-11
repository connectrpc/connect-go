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

// TestEcosystemWarnings checks that ecosystem call sites the tool can't
// reshape mechanically produce a warning naming the v2 destination. The
// mechanical reshapes (grpchealth.NewHandler, grpcreflect.NewClient) are
// covered by the golden cases under testdata/ecosystem_*.
func TestEcosystemWarnings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		importPath string
		body       string
		wantWarn   string
	}{
		{
			name:       "authn_middleware",
			importPath: "connectrpc.com/authn",
			body:       `var _ = authn.NewMiddleware(nil)`,
			wantWarn:   "authn.NewServerInterceptor",
		},
		{
			name:       "grpcreflect_handler_v1",
			importPath: "connectrpc.com/grpcreflect",
			body:       `var _, _ = grpcreflect.NewHandlerV1(nil)`,
			wantWarn:   "grpcreflect.Register",
		},
		{
			name:       "grpcreflect_handler_v1alpha",
			importPath: "connectrpc.com/grpcreflect",
			body:       `var _, _ = grpcreflect.NewHandlerV1Alpha(nil)`,
			wantWarn:   "grpcreflect.Register",
		},
		{
			name:       "grpcreflect_static_reflector",
			importPath: "connectrpc.com/grpcreflect",
			body:       `var _ = grpcreflect.NewStaticReflector("acme.user.v1.UserService")`,
			wantWarn:   "grpcreflect.Register",
		},
		{
			name:       "vanguard_transcoder",
			importPath: "connectrpc.com/vanguard",
			body:       `var _, _ = vanguard.NewTranscoder(nil)`,
			wantWarn:   "vanguard.Mount",
		},
		{
			name:       "vanguard_service",
			importPath: "connectrpc.com/vanguard",
			body:       `var _ = vanguard.NewService("acme.user.v1.UserService", nil)`,
			wantWarn:   "vanguard.Mount",
		},
		{
			name:       "vanguardgrpc_transcoder",
			importPath: "connectrpc.com/vanguard/vanguardgrpc",
			body:       `var _, _ = vanguardgrpc.NewTranscoder(nil)`,
			wantWarn:   "vanguardgrpc.NewServiceRegistrar",
		},
		{
			name:       "otelconnect_interceptor",
			importPath: "connectrpc.com/otelconnect",
			body:       `var _, _ = otelconnect.NewInterceptor()`,
			wantWarn:   "otelconnect.NewServerInterceptor or otelconnect.NewClientInterceptor",
		},
		{
			name:       "validate_interceptor_assigned",
			importPath: "connectrpc.com/validate",
			body:       `var _ = validate.NewInterceptor()`,
			wantWarn:   "validate.NewServerInterceptor or validate.NewClientInterceptor",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			src := "package p\n\nimport \"" + test.importPath + "\"\n\n" + test.body + "\n"
			_, report, err := Rewrite("input.go", []byte(src), true)
			if err != nil {
				t.Fatalf("Rewrite: %v", err)
			}
			found := false
			for _, warning := range report.Warnings {
				if warning.Rule == ruleEcosystemMigration && strings.Contains(warning.Msg, test.wantWarn) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected an %s warning containing %q; got %v", ruleEcosystemMigration, test.wantWarn, report.Warnings)
			}
		})
	}
}
