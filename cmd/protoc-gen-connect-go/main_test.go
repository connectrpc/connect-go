// Copyright 2021-2024 The Connect Authors
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
	"io"
	"os"
	"os/exec"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

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
	// Make sure empty package_suffix works alone.
	t.Run("ping.proto:package_suffix=", func(t *testing.T) {
		t.Parallel()
		req := &pluginpb.CodeGeneratorRequest{
			FileToGenerate:        []string{"connect/ping/v1/ping.proto"},
			Parameter:             ptr("package_suffix="),
			ProtoFile:             []*descriptorpb.FileDescriptorProto{pingFileDesc},
			SourceFileDescriptors: []*descriptorpb.FileDescriptorProto{pingFileDesc},
			CompilerVersion:       compilerVersion,
		}
		rsp := testGenerate(t, req)
		assert.Nil(t, rsp.Error)

		assert.Equal(t, len(rsp.File), 1)
		file := rsp.File[0]
		assert.Equal(t, file.GetName(), "connectrpc.com/connect/internal/gen/connect/ping/v1/ping.connect.go")
		assert.NotZero(t, file.GetContent())
	})
	// Make sure empty package_suffix works with another option.
	t.Run("ping.proto:package_suffix=,paths=source_relative", func(t *testing.T) {
		t.Parallel()
		req := &pluginpb.CodeGeneratorRequest{
			FileToGenerate:        []string{"connect/ping/v1/ping.proto"},
			Parameter:             ptr("package_suffix="),
			ProtoFile:             []*descriptorpb.FileDescriptorProto{pingFileDesc},
			SourceFileDescriptors: []*descriptorpb.FileDescriptorProto{pingFileDesc},
			CompilerVersion:       compilerVersion,
		}
		rsp := testGenerate(t, req)
		assert.Nil(t, rsp.Error)

		assert.Equal(t, len(rsp.File), 1)
		file := rsp.File[0]
		assert.Equal(t, file.GetName(), "connectrpc.com/connect/internal/gen/connect/ping/v1/ping.connect.go")
		assert.NotZero(t, file.GetContent())
	})
	t.Run("ping.proto:package_suffix=baz", func(t *testing.T) {
		t.Parallel()
		req := &pluginpb.CodeGeneratorRequest{
			FileToGenerate:        []string{"connect/ping/v1/ping.proto"},
			Parameter:             ptr("package_suffix=baz"),
			ProtoFile:             []*descriptorpb.FileDescriptorProto{pingFileDesc},
			SourceFileDescriptors: []*descriptorpb.FileDescriptorProto{pingFileDesc},
			CompilerVersion:       compilerVersion,
		}
		rsp := testGenerate(t, req)
		assert.Nil(t, rsp.Error)

		assert.Equal(t, len(rsp.File), 1)
		file := rsp.File[0]
		assert.Equal(t, file.GetName(), "connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1baz/ping.connect.go")
		assert.NotZero(t, file.GetContent())
	})
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
