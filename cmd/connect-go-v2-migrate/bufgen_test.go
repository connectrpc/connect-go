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

// TestRewriteBufGen exercises RewriteBufGen over inline templates. The
// file-pair golden cases live in the testscript harness (testdata/script/
// bufgen_*.txtar).
func TestRewriteBufGen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		in          string
		want        string // empty means "expect unchanged (byte-identical to in)"
		wantChanged bool
		wantWarn    string // substring expected in some warning (empty means none required)
	}{
		{
			name: "inline_strip_simple_keeps_rest",
			in: `version: v2
plugins:
  - local: protoc-gen-connect-go
    out: gen
    opt: paths=source_relative,simple=true
`,
			want: `version: v2
plugins:
  - local: protoc-gen-connect-go
    out: gen
    opt: paths=source_relative
`,
			wantChanged: true,
			wantWarn:    "reinstall the generator",
		},
		{
			name: "inline_only_simple_drops_opt_line",
			in: `version: v2
plugins:
  - local: protoc-gen-connect-go
    out: gen
    opt: simple=true
`,
			want: `version: v2
plugins:
  - local: protoc-gen-connect-go
    out: gen
`,
			wantChanged: true,
		},
		{
			name: "list_strip_simple_keeps_rest",
			in: `version: v2
plugins:
  - local: protoc-gen-connect-go
    out: gen
    opt:
      - paths=source_relative
      - simple=true
`,
			want: `version: v2
plugins:
  - local: protoc-gen-connect-go
    out: gen
    opt:
      - paths=source_relative
`,
			wantChanged: true,
		},
		{
			name: "list_only_simple_drops_opt_header",
			in: `version: v2
plugins:
  - local: protoc-gen-connect-go
    out: gen
    opt:
      - simple=true
`,
			want: `version: v2
plugins:
  - local: protoc-gen-connect-go
    out: gen
`,
			wantChanged: true,
		},
		{
			name: "remote_plugin_pinned_and_strips",
			in: `version: v2
plugins:
  - remote: buf.build/connectrpc/go:v1.18.1
    opt: simple=true
`,
			want: `version: v2
plugins:
  - remote: buf.build/connectrpc/go:v2.0.0
`,
			wantChanged: true,
		},
		{
			name: "remote_plugin_unversioned_pinned",
			in: `version: v2
plugins:
  - remote: buf.build/connectrpc/go
    out: gen
`,
			want: `version: v2
plugins:
  - remote: buf.build/connectrpc/go:v2.0.0
    out: gen
`,
			wantChanged: true,
		},
		{
			// `revision` only applies to remote plugins, so it is dropped.
			name: "remote_plugin_revision_dropped",
			in: `version: v2
plugins:
  - remote: buf.build/connectrpc/go:v1.18.1
    revision: 1
    out: gen
`,
			want: `version: v2
plugins:
  - remote: buf.build/connectrpc/go:v2.0.0
    out: gen
`,
			wantChanged: true,
		},
		{
			// The plugin key need not be the item's first key.
			name: "remote_plugin_key_not_first",
			in: `version: v2
plugins:
  - out: gen
    remote: buf.build/connectrpc/go:v1.18.1
    opt: simple=true
`,
			want: `version: v2
plugins:
  - out: gen
    remote: buf.build/connectrpc/go:v2.0.0
`,
			wantChanged: true,
		},
		{
			name: "v1_plugin_key_not_first",
			in: `version: v1
plugins:
  - out: gen
    plugin: buf.build/connectrpc/go:v1.18.1
`,
			want: `version: v1
plugins:
  - out: gen
    plugin: buf.build/connectrpc/go:v2.0.0
`,
			wantChanged: true,
		},
		{
			name: "local_plugin_key_not_first_strips_simple",
			in: `version: v2
plugins:
  - out: gen
    opt: simple=true
    local: protoc-gen-connect-go
`,
			want: `version: v2
plugins:
  - out: gen
    local: protoc-gen-connect-go
`,
			wantChanged: true,
			wantWarn:    "reinstall the generator",
		},
		{
			// Removing a key on the dash line moves the next key up.
			name: "opt_first_hoists_next_key",
			in: `version: v2
plugins:
  - opt: simple=true
    local: protoc-gen-connect-go
`,
			want: `version: v2
plugins:
  - local: protoc-gen-connect-go
`,
			wantChanged: true,
			wantWarn:    "reinstall the generator",
		},
		{
			name: "revision_first_hoists_next_key",
			in: `version: v2
plugins:
  - revision: 1
    remote: buf.build/connectrpc/go:v1.18.1
    out: gen
`,
			want: `version: v2
plugins:
  - remote: buf.build/connectrpc/go:v2.0.0
    out: gen
`,
			wantChanged: true,
		},
		{
			name: "opt_list_first_hoists_next_key",
			in: `version: v2
plugins:
  - opt:
      - simple=true
    out: gen
    local: protoc-gen-connect-go
`,
			want: `version: v2
plugins:
  - out: gen
    local: protoc-gen-connect-go
`,
			wantChanged: true,
		},
		{
			name: "opt_list_first_keeps_other_options",
			in: `version: v2
plugins:
  - opt:
      - paths=source_relative
      - simple=true
    local: protoc-gen-connect-go
`,
			want: `version: v2
plugins:
  - opt:
      - paths=source_relative
    local: protoc-gen-connect-go
`,
			wantChanged: true,
		},
		{
			name: "flow_mapping_pinned",
			in: `version: v2
plugins:
  - {remote: buf.build/connectrpc/go:v1.18.1, out: gen}
`,
			want: `version: v2
plugins:
  - {remote: buf.build/connectrpc/go:v2.0.0, out: gen}
`,
			wantChanged: true,
		},
		{
			name: "quoted_and_commented_ref_pinned",
			in: `version: v2
plugins:
  - remote: "buf.build/connectrpc/go:v1.18.1" # pinned
    out: gen
`,
			want: `version: v2
plugins:
  - remote: "buf.build/connectrpc/go:v2.0.0" # pinned
    out: gen
`,
			wantChanged: true,
		},
		{
			// A flow opt list can't lose an item line by line.
			name: "flow_opt_list_with_simple_warns",
			in: `version: v2
plugins:
  - local: protoc-gen-connect-go
    opt: [paths=source_relative, simple=true]
`,
			want:        "",
			wantChanged: false,
			wantWarn:    "remove the v1 `simple` option by hand",
		},
		{
			name: "invalid_yaml_warns",
			in: `version: v2
plugins:
  - local: protoc-gen-connect-go
   out: [
`,
			want:        "",
			wantChanged: false,
			wantWarn:    "could not parse the template",
		},
		{
			// A key nested deeper than the item's own keys is not the plugin.
			name: "nested_key_ignored",
			in: `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go
    out: gen
    opt:
      - remote: buf.build/connectrpc/go:v1.18.1
`,
			want:        "",
			wantChanged: false,
		},
		{
			name: "remote_plugin_already_v2_is_noop",
			in: `version: v2
plugins:
  - remote: buf.build/connectrpc/go:v2.0.0
    out: gen
`,
			want:        "",
			wantChanged: false,
		},
		{
			// v2 folds the simple API into the default generator, so the
			// gosimple plugin migrates onto the same local plugin.
			name: "remote_gosimple_pinned",
			in: `version: v2
plugins:
  - remote: buf.build/connectrpc/gosimple:v1.18.1
    out: gen
`,
			want: `version: v2
plugins:
  - remote: buf.build/connectrpc/go:v2.0.0
    out: gen
`,
			wantChanged: true,
		},
		{
			name: "remote_gosimple_unversioned_pinned",
			in: `version: v2
plugins:
  - remote: buf.build/connectrpc/gosimple
    out: gen
`,
			want: `version: v2
plugins:
  - remote: buf.build/connectrpc/go:v2.0.0
    out: gen
`,
			wantChanged: true,
		},
		{
			name: "v1_plugin_gosimple_pinned",
			in: `version: v1
plugins:
  - plugin: buf.build/connectrpc/gosimple:v1.18.1
    out: gen
`,
			want: `version: v1
plugins:
  - plugin: buf.build/connectrpc/go:v2.0.0
    out: gen
`,
			wantChanged: true,
		},
		{
			// Private BSR instances use the same plugin path under another host,
			// and are pinned too.
			name: "remote_private_host_pinned",
			in: `version: v2
plugins:
  - remote: buf.example.com/connectrpc/go:v1.18.1
    out: gen
`,
			want: `version: v2
plugins:
  - remote: buf.example.com/connectrpc/go:v2.0.0
    out: gen
`,
			wantChanged: true,
		},
		{
			name: "remote_private_host_gosimple_pinned",
			in: `version: v2
plugins:
  - remote: bsr.internal.acme.dev/connectrpc/gosimple:v1.18.1
    out: gen
`,
			want: `version: v2
plugins:
  - remote: bsr.internal.acme.dev/connectrpc/go:v2.0.0
    out: gen
`,
			wantChanged: true,
		},
		{
			name: "remote_private_host_already_v2_is_noop",
			in: `version: v2
plugins:
  - remote: buf.example.com/connectrpc/go:v2.0.0
    out: gen
`,
			want:        "",
			wantChanged: false,
		},
		{
			// A hostless reference is not a BSR plugin and must be left alone.
			name: "remote_hostless_ref_ignored",
			in: `version: v2
plugins:
  - remote: connectrpc/go:v1.18.1
    out: gen
`,
			want:        "",
			wantChanged: false,
		},
		{
			name: "remote_plugin_pinned",
			in: `version: v2
plugins:
  - remote: buf.build/connectrpc/go:v1.18.1
    out: gen
`,
			want: `version: v2
plugins:
  - remote: buf.build/connectrpc/go:v2.0.0
    out: gen
`,
			wantChanged: true,
		},
		{
			name: "gotool_warns_about_gomod_and_strips",
			in: `version: v2
plugins:
  - local: [go, tool, protoc-gen-connect-go]
    out: gen
    opt: paths=source_relative,simple=true
`,
			want: `version: v2
plugins:
  - local: [go, tool, protoc-gen-connect-go]
    out: gen
    opt: paths=source_relative
`,
			wantChanged: true,
			wantWarn:    "go.mod",
		},
		{
			name: "no_simple_local_warns_only_no_change",
			in: `version: v2
plugins:
  - local: protoc-gen-connect-go
    out: gen
    opt: paths=source_relative
`,
			want:        "", // unchanged
			wantChanged: false,
			wantWarn:    "reinstall the generator",
		},
		{
			name: "preserves_other_plugins_and_comments",
			in: `version: v2
plugins:
  - local: protoc-gen-go # base types
    out: gen
    opt: paths=source_relative
  - local: protoc-gen-connect-go
    out: gen
    opt: paths=source_relative,simple=true
`,
			want: `version: v2
plugins:
  - local: protoc-gen-go # base types
    out: gen
    opt: paths=source_relative
  - local: protoc-gen-connect-go
    out: gen
    opt: paths=source_relative
`,
			wantChanged: true,
		},
		{
			name: "no_connect_plugin_is_noop",
			in: `version: v2
plugins:
  - local: protoc-gen-go
    out: gen
    opt: paths=source_relative,simple=true
`,
			want:        "",
			wantChanged: false,
		},
		{
			name: "v1_name_local_warns_reinstall",
			in: `version: v1
managed:
  enabled: true
  go_package_prefix:
    default: connect-examples-go/internal/gen
plugins:
  - name: go
    out: internal/gen
    opt: paths=source_relative
  - name: connect-go
    out: internal/gen
    opt: paths=source_relative
`,
			want:        "", // unchanged
			wantChanged: false,
			wantWarn:    "reinstall the generator",
		},
		{
			name: "v1_name_strip_simple",
			in: `version: v1
plugins:
  - name: connect-go
    out: gen
    opt: paths=source_relative,simple=true
`,
			want: `version: v1
plugins:
  - name: connect-go
    out: gen
    opt: paths=source_relative
`,
			wantChanged: true,
			wantWarn:    "reinstall the generator",
		},
		{
			name: "v1_plugin_local_warns_reinstall",
			in: `version: v1
plugins:
  - plugin: connect-go
    out: gen
    opt: paths=source_relative
`,
			want:        "", // unchanged
			wantChanged: false,
			wantWarn:    "reinstall the generator",
		},
		{
			name: "v1_plugin_remote_pinned_and_strips",
			in: `version: v1
plugins:
  - plugin: buf.build/connectrpc/go:v1.18.1
    out: gen
    opt: simple=true
`,
			want: `version: v1
plugins:
  - plugin: buf.build/connectrpc/go:v2.0.0
    out: gen
`,
			wantChanged: true,
		},
		{
			name: "v1_plugin_remote_unversioned_pinned",
			in: `version: v1
plugins:
  - plugin: buf.build/connectrpc/go
    out: gen
`,
			want: `version: v1
plugins:
  - plugin: buf.build/connectrpc/go:v2.0.0
    out: gen
`,
			wantChanged: true,
		},
		{
			name: "v1_plugin_remote_already_v2_is_noop",
			in: `version: v1
plugins:
  - plugin: buf.build/connectrpc/go:v2.0.0
    out: gen
`,
			want:        "",
			wantChanged: false,
		},
		{
			name: "v1_path_go_run_warns_gomod",
			in: `version: v1
plugins:
  - name: connect-go
    out: gen
    opt: paths=source_relative,simple=true
    path: [go, run, connectrpc.com/connect/cmd/protoc-gen-connect-go]
`,
			want: `version: v1
plugins:
  - name: connect-go
    out: gen
    opt: paths=source_relative
    path: [go, run, connectrpc.com/connect/cmd/protoc-gen-connect-go]
`,
			wantChanged: true,
			wantWarn:    "go.mod",
		},
		{
			name: "v1_path_binary_keeps_reinstall_warning",
			in: `version: v1
plugins:
  - name: connect-go
    out: gen
    path: bin/protoc-gen-connect-go
`,
			want:        "", // unchanged
			wantChanged: false,
			wantWarn:    "reinstall the generator",
		},
		{
			name: "v1_name_go_is_noop",
			in: `version: v1
plugins:
  - name: go
    out: gen
    opt: paths=source_relative,simple=true
`,
			want:        "",
			wantChanged: false,
		},
		{
			name: "simple_false_also_stripped",
			in: `version: v2
plugins:
  - local: protoc-gen-connect-go
    opt: simple=false,paths=source_relative
`,
			want: `version: v2
plugins:
  - local: protoc-gen-connect-go
    opt: paths=source_relative
`,
			wantChanged: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, report, err := RewriteBufGen("buf.gen.yaml", []byte(test.in))
			if err != nil {
				t.Fatalf("RewriteBufGen: %v", err)
			}
			if report.Changed != test.wantChanged {
				t.Errorf("Changed = %v, want %v (report %s)", report.Changed, test.wantChanged, report.Summary())
			}
			want := test.want
			if want == "" {
				want = test.in
			}
			if string(got) != want {
				t.Errorf("output mismatch\n--- want ---\n%s\n--- got ---\n%s", want, string(got))
			}
			if test.wantWarn != "" {
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
			}
			// Idempotency: a second pass must not change the (already-migrated)
			// output bytes.
			got2, _, err := RewriteBufGen("buf.gen.yaml", got)
			if err != nil {
				t.Fatalf("second-pass RewriteBufGen: %v", err)
			}
			if string(got2) != string(got) {
				t.Errorf("not idempotent; second pass changed output\n--- first ---\n%s\n--- second ---\n%s", string(got), string(got2))
			}
		})
	}
}
