// Copyright 2021-2025 The Connect Authors
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
	"bytes"
	"context"
	"embed"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"

	defaultpackage "connectrpc.com/connect/cmd/protoc-gen-connect-go/internal/testdata/defaultpackage/gen"
	defaultpackageconnect "connectrpc.com/connect/cmd/protoc-gen-connect-go/internal/testdata/defaultpackage/gen/genconnect"
	diffpackage "connectrpc.com/connect/cmd/protoc-gen-connect-go/internal/testdata/diffpackage/gen"
	diffpackagediff "connectrpc.com/connect/cmd/protoc-gen-connect-go/internal/testdata/diffpackage/gen/gendiff"
	noservice "connectrpc.com/connect/cmd/protoc-gen-connect-go/internal/testdata/noservice/gen"
	samepackage "connectrpc.com/connect/cmd/protoc-gen-connect-go/internal/testdata/samepackage/gen"

	simple "connectrpc.com/connect/cmd/protoc-gen-connect-go/internal/testdata/simple/gen"
	_ "connectrpc.com/connect/cmd/protoc-gen-connect-go/internal/testdata/v1beta1service/gen"
)

//go:embed internal/testdata
var testdata embed.FS

func TestVersion(t *testing.T) {
	t.Parallel()
	stdout, stderr, exitCode := testRunProtocGenGo(t, nil, "--version")
	assert.Equal(t, stdout.String(), connect.Version+"\n")
	assert.Equal(t, stderr.String(), "")
	assert.Equal(t, exitCode, 0)
}

func TestGenerate(t *testing.T) {
	t.Parallel()
	pingFileDesc := protodesc.ToFileDescriptorProto(pingv1.File_connect_ping_v1_ping_proto)
	compilerVersion := &pluginpb.Version{
		Major:  ptr(int32(0)),
		Minor:  ptr(int32(0)),
		Patch:  ptr(int32(1)),
		Suffix: ptr("test"),
	}
	t.Run("ping.proto", func(t *testing.T) {
		t.Parallel()
		req := &pluginpb.CodeGeneratorRequest{
			FileToGenerate:        []string{"connect/ping/v1/ping.proto"},
			Parameter:             nil,
			ProtoFile:             []*descriptorpb.FileDescriptorProto{pingFileDesc},
			SourceFileDescriptors: []*descriptorpb.FileDescriptorProto{pingFileDesc},
			CompilerVersion:       compilerVersion,
		}
		rsp := testGenerate(t, req)
		assert.Nil(t, rsp.Error)

		assert.Equal(t, rsp.GetSupportedFeatures(), 3)
		assert.Equal(t, rsp.GetMinimumEdition(), int32(descriptorpb.Edition_EDITION_PROTO2))
		assert.Equal(t, rsp.GetMaximumEdition(), int32(descriptorpb.Edition_EDITION_2023))

		assert.Equal(t, len(rsp.File), 1)
		file := rsp.File[0]
		assert.Equal(t, file.GetName(), "connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect/ping.connect.go")
		assert.NotZero(t, file.GetContent())
	})
	t.Run("defaultpackage.proto", func(t *testing.T) {
		t.Parallel()
		defaultPackageFileDesc := protodesc.ToFileDescriptorProto(defaultpackage.File_defaultpackage_proto)
		req := &pluginpb.CodeGeneratorRequest{
			FileToGenerate:        []string{"defaultpackage.proto"},
			Parameter:             nil,
			ProtoFile:             []*descriptorpb.FileDescriptorProto{defaultPackageFileDesc},
			SourceFileDescriptors: []*descriptorpb.FileDescriptorProto{defaultPackageFileDesc},
			CompilerVersion:       compilerVersion,
		}
		rsp := testGenerate(t, req)
		assert.Nil(t, rsp.Error)

		assert.Equal(t, rsp.GetSupportedFeatures(), 3)
		assert.Equal(t, rsp.GetMinimumEdition(), int32(descriptorpb.Edition_EDITION_PROTO2))
		assert.Equal(t, rsp.GetMaximumEdition(), int32(descriptorpb.Edition_EDITION_2023))

		assert.Equal(t, len(rsp.File), 1)
		file := rsp.File[0]
		assert.Equal(t, file.GetName(), "connectrpc.com/connect/cmd/protoc-gen-connect-go/internal/testdata/defaultpackage/gen/genconnect/defaultpackage.connect.go")
		assert.NotZero(t, file.GetContent())
		testCmpToTestdata(t, file.GetContent(), "internal/testdata/defaultpackage/gen/genconnect/defaultpackage.connect.go")
	})
	// Check generated code into a the same package.
	t.Run("samepackage.proto", func(t *testing.T) {
		t.Parallel()
		samePackageFileDesc := protodesc.ToFileDescriptorProto(samepackage.File_samepackage_proto)
		req := &pluginpb.CodeGeneratorRequest{
			FileToGenerate:        []string{"samepackage.proto"},
			Parameter:             ptr("package_suffix"),
			ProtoFile:             []*descriptorpb.FileDescriptorProto{samePackageFileDesc},
			SourceFileDescriptors: []*descriptorpb.FileDescriptorProto{samePackageFileDesc},
			CompilerVersion:       compilerVersion,
		}
		rsp := testGenerate(t, req)
		assert.Nil(t, rsp.Error)

		assert.Equal(t, len(rsp.File), 1)
		file := rsp.File[0]
		assert.Equal(t, file.GetName(), "connectrpc.com/connect/cmd/protoc-gen-connect-go/internal/testdata/samepackage/gen/samepackage.connect.go")
		assert.NotZero(t, file.GetContent())
		testCmpToTestdata(t, file.GetContent(), "internal/testdata/samepackage/gen/samepackage.connect.go")
	})
	// Check generated code into a different subpackage.
	t.Run("diffpackage.proto", func(t *testing.T) {
		t.Parallel()
		diffPackageFileDesc := protodesc.ToFileDescriptorProto(diffpackage.File_diffpackage_proto)
		req := &pluginpb.CodeGeneratorRequest{
			FileToGenerate:        []string{"diffpackage.proto"},
			Parameter:             ptr("package_suffix=diff"),
			ProtoFile:             []*descriptorpb.FileDescriptorProto{diffPackageFileDesc},
			SourceFileDescriptors: []*descriptorpb.FileDescriptorProto{diffPackageFileDesc},
			CompilerVersion:       compilerVersion,
		}
		rsp := testGenerate(t, req)
		assert.Nil(t, rsp.Error)

		assert.Equal(t, len(rsp.File), 1)
		file := rsp.File[0]
		assert.Equal(t, file.GetName(), "connectrpc.com/connect/cmd/protoc-gen-connect-go/internal/testdata/diffpackage/gen/gendiff/diffpackage.connect.go")
		assert.NotZero(t, file.GetContent())
		testCmpToTestdata(t, file.GetContent(), "internal/testdata/diffpackage/gen/gendiff/diffpackage.connect.go")
	})
	// Validate package_suffix option.
	t.Run("ping.proto:invalid_package_suffix", func(t *testing.T) {
		t.Parallel()
		req := &pluginpb.CodeGeneratorRequest{
			FileToGenerate:        []string{"connect/ping/v1/ping.proto"},
			Parameter:             ptr("package_suffix=1234"),
			ProtoFile:             []*descriptorpb.FileDescriptorProto{pingFileDesc},
			SourceFileDescriptors: []*descriptorpb.FileDescriptorProto{pingFileDesc},
			CompilerVersion:       compilerVersion,
		}
		rsp := testGenerate(t, req)
		assert.NotNil(t, rsp.Error)
		assert.Equal(t, *rsp.Error, `package_suffix "1234" is not a valid Go identifier`)
	})
	// Check no service in the file.
	t.Run("noservice.proto", func(t *testing.T) {
		t.Parallel()
		noServiceFileDesc := protodesc.ToFileDescriptorProto(noservice.File_noservice_proto)
		req := &pluginpb.CodeGeneratorRequest{
			FileToGenerate:        []string{"noservice.proto"},
			ProtoFile:             []*descriptorpb.FileDescriptorProto{noServiceFileDesc},
			SourceFileDescriptors: []*descriptorpb.FileDescriptorProto{noServiceFileDesc},
			CompilerVersion:       compilerVersion,
		}
		rsp := testGenerate(t, req)
		assert.Nil(t, rsp.Error)
		assert.Equal(t, len(rsp.File), 0)
	})
	t.Run("simple.proto", func(t *testing.T) {
		t.Parallel()
		simpleFileDesc := protodesc.ToFileDescriptorProto(simple.File_simple_proto)
		req := &pluginpb.CodeGeneratorRequest{
			FileToGenerate:        []string{"simple.proto"},
			Parameter:             ptr("api=simple"),
			ProtoFile:             []*descriptorpb.FileDescriptorProto{simpleFileDesc},
			SourceFileDescriptors: []*descriptorpb.FileDescriptorProto{simpleFileDesc},
			CompilerVersion:       compilerVersion,
		}
		rsp := testGenerate(t, req)
		assert.Nil(t, rsp.Error)

		assert.Equal(t, len(rsp.File), 1)
		file := rsp.File[0]
		assert.Equal(t, file.GetName(), "connectrpc.com/connect/cmd/protoc-gen-connect-go/internal/testdata/simple/gen/genconnect/simple.connect.go")
		assert.NotZero(t, file.GetContent())

		testCmpToTestdata(t, file.GetContent(), "internal/testdata/simple/gen/genconnect/simple.connect.go")
	})
	t.Run("invalid api option", func(t *testing.T) {
		t.Parallel()
		simpleFileDesc := protodesc.ToFileDescriptorProto(simple.File_simple_proto)
		req := &pluginpb.CodeGeneratorRequest{
			FileToGenerate:        []string{"simple.proto"},
			Parameter:             ptr("api=foo"),
			ProtoFile:             []*descriptorpb.FileDescriptorProto{simpleFileDesc},
			SourceFileDescriptors: []*descriptorpb.FileDescriptorProto{simpleFileDesc},
			CompilerVersion:       compilerVersion,
		}
		rsp := testGenerate(t, req)
		assert.NotNil(t, rsp.Error)
		assert.Equal(t, *rsp.Error, `'simple' is the only valid value when specifying an api type`)
	})
	t.Run("empty api type defaults to wrapped", func(t *testing.T) {
		t.Parallel()
		req := &pluginpb.CodeGeneratorRequest{
			FileToGenerate:        []string{"connect/ping/v1/ping.proto"},
			Parameter:             ptr("api="),
			ProtoFile:             []*descriptorpb.FileDescriptorProto{pingFileDesc},
			SourceFileDescriptors: []*descriptorpb.FileDescriptorProto{pingFileDesc},
			CompilerVersion:       compilerVersion,
		}
		rsp := testGenerate(t, req)
		assert.Nil(t, rsp.Error)

		assert.Equal(t, rsp.GetSupportedFeatures(), 3)
		assert.Equal(t, rsp.GetMinimumEdition(), int32(descriptorpb.Edition_EDITION_PROTO2))
		assert.Equal(t, rsp.GetMaximumEdition(), int32(descriptorpb.Edition_EDITION_2023))

		assert.Equal(t, len(rsp.File), 1)
		file := rsp.File[0]
		assert.Equal(t, file.GetName(), "connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect/ping.connect.go")
		assert.NotZero(t, file.GetContent())
	})
}

func TestClientHandler(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("defaultpackage.proto", func(t *testing.T) {
		t.Parallel()
		svc := testDefaultPackageService{}
		mux := http.NewServeMux()
		mux.Handle(defaultpackageconnect.NewTestServiceHandler(svc))
		server := httptest.NewServer(mux)
		client := defaultpackageconnect.NewTestServiceClient(server.Client(), server.URL)
		rsp, err := client.Method(ctx, connect.NewRequest(&defaultpackage.Request{}))
		assert.Nil(t, err)
		assert.NotNil(t, rsp)
	})
	t.Run("diffpackage.proto", func(t *testing.T) {
		t.Parallel()
		svc := testDiffPackageService{}
		mux := http.NewServeMux()
		mux.Handle(diffpackagediff.NewTestServiceHandler(svc))
		server := httptest.NewServer(mux)
		client := diffpackagediff.NewTestServiceClient(server.Client(), server.URL)
		rsp, err := client.Method(ctx, connect.NewRequest(&diffpackage.Request{}))
		assert.Nil(t, err)
		assert.NotNil(t, rsp)
	})
	t.Run("samepackage.proto", func(t *testing.T) {
		t.Parallel()
		svc := testSamePackageService{}
		mux := http.NewServeMux()
		mux.Handle(samepackage.NewTestServiceHandler(svc))
		server := httptest.NewServer(mux)
		client := samepackage.NewTestServiceClient(server.Client(), server.URL)
		rsp, err := client.Method(ctx, connect.NewRequest(&samepackage.Request{}))
		assert.Nil(t, err)
		assert.NotNil(t, rsp)
	})
}

func testCmpToTestdata(t *testing.T, content, path string) {
	t.Helper()
	b, err := testdata.ReadFile(path)
	assert.Nil(t, err)
	// Strip the copyright header and generated by line.
	fileContent := string(b)
	if codeGenerateIndex := strings.Index(fileContent, "// Code generated by"); codeGenerateIndex != -1 {
		fileContent = fileContent[codeGenerateIndex:]
		fileContent = strings.Replace(fileContent, "Code generated by protoc-gen-connect-go.", "Code generated by main.", 1)
	}
	if runtime.GOOS == "windows" {
		fileContent = strings.ReplaceAll(fileContent, "\r\n", "\n")
	}
	assert.Zero(t, cmp.Diff(content, fileContent))
}

func testGenerate(t *testing.T, req *pluginpb.CodeGeneratorRequest) *pluginpb.CodeGeneratorResponse {
	t.Helper()

	inputBytes, err := proto.Marshal(req)
	assert.Nil(t, err)

	stdout, stderr, exitCode := testRunProtocGenGo(t, bytes.NewReader(inputBytes))
	assert.Equal(t, exitCode, 0)
	assert.Equal(t, stderr.String(), "")
	assert.True(t, len(stdout.Bytes()) > 0)

	var output pluginpb.CodeGeneratorResponse
	assert.Nil(t, proto.Unmarshal(stdout.Bytes(), &output))
	return &output
}

func testRunProtocGenGo(t *testing.T, stdin io.Reader, args ...string) (stdout, stderr *bytes.Buffer, exitCode int) {
	t.Helper()

	stdout = &bytes.Buffer{}
	stderr = &bytes.Buffer{}
	args = append([]string{"run", "main.go"}, args...)

	cmd := exec.Command("go", args...)
	cmd.Env = os.Environ()
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	assert.Nil(t, cmd.Run(), assert.Sprintf("Run go %v", args))
	exitCode = cmd.ProcessState.ExitCode()
	return stdout, stderr, exitCode
}

func ptr[T any](v T) *T {
	return &v
}

type testDefaultPackageService struct {
	defaultpackageconnect.UnimplementedTestServiceHandler
}

func (testDefaultPackageService) Method(context.Context, *connect.Request[defaultpackage.Request]) (*connect.Response[defaultpackage.Response], error) {
	return connect.NewResponse(&defaultpackage.Response{}), nil
}

type testDiffPackageService struct {
	diffpackagediff.UnimplementedTestServiceHandler
}

func (testDiffPackageService) Method(context.Context, *connect.Request[diffpackage.Request]) (*connect.Response[diffpackage.Response], error) {
	return connect.NewResponse(&diffpackage.Response{}), nil
}

type testSamePackageService struct {
	samepackage.UnimplementedTestServiceHandler
}

func (testSamePackageService) Method(context.Context, *connect.Request[samepackage.Request]) (*connect.Response[samepackage.Response], error) {
	return connect.NewResponse(&samepackage.Response{}), nil
}
