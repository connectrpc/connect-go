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

// TestStreamWarnings checks the streaming reshapes that the tool cannot perform
// mechanically and instead surfaces as warnings: the handler stream parameter
// type, whose generated v2 name the AST rewriter can't infer.
func TestStreamWarnings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		body     string
		wantWarn string
	}{
		{
			name: "handler_param_type",
			body: `func h(ctx context.Context, stream *connect.BidiStream[in, out]) error {
	return stream.Send(&out{})
}`,
			wantWarn: "connect.BidiStream",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			src := "package p\n\nimport (\n\t\"context\"\n\n\t\"connectrpc.com/connect\"\n)\n\ntype in struct{}\ntype out struct{}\n\n" + test.body + "\n"
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

// TestHandlerStreamLookupDisambiguation checks that when two services share an
// RPC name and message types (a querier and an ingester exposing the same
// streaming RPC, say), the handler's receiver type breaks the tie, and an
// unrecognized receiver leaves the parameter a warning rather than guessing.
func TestHandlerStreamLookupDisambiguation(t *testing.T) {
	t.Parallel()
	msgs := []string{"MergeProfilesStacktracesRequest", "MergeProfilesStacktracesResponse"}
	resolver := &handlerStreamResolver{
		types: []handlerStreamType{
			{pkgName: "ingestv1connect", name: "IngesterServiceMergeProfilesStacktracesServerStream", messages: msgs},
			{pkgName: "ingestv1connect", name: "QuerierServiceMergeProfilesStacktracesServerStream", messages: msgs},
		},
	}
	// No receiver hint: ambiguous. The lookup fails and returns both candidates
	// so the caller can name them.
	if _, ambiguous, ok := resolver.lookup("MergeProfilesStacktraces", "", msgs); ok {
		t.Errorf("expected ambiguous lookup to fail without a receiver hint")
	} else if len(ambiguous) != 2 {
		t.Errorf("expected 2 ambiguous candidates; got %d", len(ambiguous))
	}
	// The receiver type names the service, breaking the tie.
	for recv, want := range map[string]string{
		"Ingester": "IngesterServiceMergeProfilesStacktracesServerStream",
		"querier":  "QuerierServiceMergeProfilesStacktracesServerStream", // case-insensitive
	} {
		got, ambiguous, ok := resolver.lookup("MergeProfilesStacktraces", recv, msgs)
		if !ok || got.name != want {
			t.Errorf("receiver %q should resolve %q; got %q ok=%v", recv, want, got.name, ok)
		}
		if len(ambiguous) != 0 {
			t.Errorf("a resolved lookup should report no ambiguity; got %v", ambiguous)
		}
	}
	// An unrecognized receiver stays ambiguous.
	if _, ambiguous, ok := resolver.lookup("MergeProfilesStacktraces", "Server", msgs); ok {
		t.Errorf("expected unrecognized receiver to remain ambiguous")
	} else if len(ambiguous) != 2 {
		t.Errorf("expected 2 ambiguous candidates for an unrecognized receiver; got %d", len(ambiguous))
	}
	// A unique match still resolves without any receiver hint.
	single := &handlerStreamResolver{types: []handlerStreamType{
		{name: "IngesterServicePushServerStream", messages: []string{"PushRequest", "PushResponse"}},
	}}
	if _, _, ok := single.lookup("Push", "", []string{"PushRequest", "PushResponse"}); !ok {
		t.Errorf("unique match should resolve without a receiver hint")
	}
}

// TestStreamParamAmbiguousWarning checks that when a handler's stream parameter
// matches several services' generated stream types and the receiver doesn't
// name one of them (a shared internal helper, say *Store), the warning says it
// is ambiguous and names the candidates, rather than the generic "its v2 type
// is the generated handler stream type".
func TestStreamParamAmbiguousWarning(t *testing.T) {
	t.Parallel()
	resolver := &handlerStreamResolver{
		types: []handlerStreamType{
			{pkgPath: "x/ingestv1connect", pkgName: "ingestv1connect", name: "IngesterServicePushServerStream", messages: []string{"in", "out"}},
			{pkgPath: "x/querierv1connect", pkgName: "querierv1connect", name: "QuerierServicePushServerStream", messages: []string{"in", "out"}},
		},
	}
	src := `package p

import (
	"context"

	"connectrpc.com/connect"
)

type in struct{}
type out struct{}
type store struct{}

func (s *store) Push(ctx context.Context, stream *connect.BidiStream[in, out]) error {
	return stream.Send(&out{})
}
`
	_, report, err := Rewrite("input.go", []byte(src), true, withHandlerStreams(resolver))
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	var warning *Warning
	for i := range report.Warnings {
		if report.Warnings[i].Rule == ruleStreamParamAmbiguous {
			warning = &report.Warnings[i]
			break
		}
	}
	if warning == nil {
		t.Fatalf("expected a %s warning; got %v", ruleStreamParamAmbiguous, report.Warnings)
	}
	for _, want := range []string{
		"matches multiple v2 handler stream types",
		"ingestv1connect.IngesterServicePushServerStream",
		"querierv1connect.QuerierServicePushServerStream",
	} {
		if !strings.Contains(warning.Msg, want) {
			t.Errorf("ambiguity warning missing %q; got %q", want, warning.Msg)
		}
	}
}
