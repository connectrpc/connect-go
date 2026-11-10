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
	"testing"
)

func TestSDKV2Path(t *testing.T) {
	t.Parallel()
	sdkModules := []string{
		"buf.build/gen/go/acme/user/connectrpc/go",
		"buf.build/gen/go/acme/order/connectrpc/gosimple",
	}
	tests := []struct {
		importPath string
		want       string
		wantOK     bool
	}{
		{
			importPath: "buf.build/gen/go/acme/user/connectrpc/go/acme/user/v1/userv1connect",
			want:       "buf.build/gen/go/acme/user/connectrpc/go/v2/acme/user/v1/userv1connect",
			wantOK:     true,
		},
		{
			importPath: "buf.build/gen/go/acme/user/connectrpc/go",
			want:       "buf.build/gen/go/acme/user/connectrpc/go/v2",
			wantOK:     true,
		},
		{
			importPath: "buf.build/gen/go/acme/order/connectrpc/gosimple/acme/order/v1/orderv1connect",
			want:       "buf.build/gen/go/acme/order/connectrpc/go/v2/acme/order/v1/orderv1connect",
			wantOK:     true,
		},
		{
			importPath: "buf.build/gen/go/acme/user/connectrpc/goextra/foo",
		},
		{
			importPath: "buf.build/gen/go/acme/user/protocolbuffers/go/acme/user/v1",
		},
	}
	for _, test := range tests {
		t.Run(test.importPath, func(t *testing.T) {
			t.Parallel()
			got, ok := sdkV2Path(test.importPath, sdkModules)
			if got != test.want || ok != test.wantOK {
				t.Errorf("sdkV2Path() = %q, %v, want %q, %v", got, ok, test.want, test.wantOK)
			}
		})
	}
}

func TestRewriteSDKImports(t *testing.T) {
	t.Parallel()
	src := `package app

import (
	"buf.build/gen/go/acme/user/connectrpc/go/acme/user/v1/userv1connect"
	userv1 "buf.build/gen/go/acme/user/protocolbuffers/go/acme/user/v1"
)

var _ userv1connect.UserServiceClient
var _ userv1.User
`
	want := `package app

import (
	"buf.build/gen/go/acme/user/connectrpc/go/v2/acme/user/v1/userv1connect"
	userv1 "buf.build/gen/go/acme/user/protocolbuffers/go/acme/user/v1"
)

var _ userv1connect.UserServiceClient
var _ userv1.User
`
	rewrite := RewriteSDKImports([]string{"buf.build/gen/go/acme/user/connectrpc/go"})
	got, report, err := rewrite("app.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("output mismatch\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
	if report.Counts["import_sdk_v2"] != 1 {
		t.Errorf("import_sdk_v2 = %d, want 1", report.Counts["import_sdk_v2"])
	}
}
